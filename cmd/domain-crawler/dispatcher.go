package main

// Host-aware dispatcher for concurrent discovery (A2, #133).
//
// The dispatcher owns EVERY piece of per-host politeness state for one BFS
// cycle and is the only goroutine that touches it, so the politeness floors
// hold GLOBALLY across workers without any locks:
//
//   - a worker only ever receives a URL whose host is ELIGIBLE: at least
//     hostDelay (or the host's robots Crawl-delay, if longer) since that
//     host's last completed request, and fewer than perHostCap pages fetched
//     this cycle. Workers never block on politeness — they only get eligible
//     work. Different hosts proceed in parallel; the same host is serialized
//     (at most one in-flight job per host: the inFlight flag).
//   - robots.txt is fetched by a worker (so a cycle that starts with hundreds
//     of new hosts ramps to full parallelism instead of serializing on the
//     dispatcher), but the dispatcher issues at most one robots job per host
//     per cycle — the inFlight flag doubles as the singleflight guard, so
//     concurrency can never cause duplicate robots fetches.
//   - netguard.Control stays on the shared httpClient's dial path: every
//     fetch, robots or page, is still address-checked at connect time.
//   - domain-crawler has no per-server-IP / per-ASN pacing layer (that lives
//     in website-crawler); every pacing layer it DOES have is carried forward
//     unchanged: ≥2s/host, ≤25 pages/host/cycle, robots always, netguard
//     always. Concurrency changes the execution model, NOT the floors.
//
// The dispatcher never does network I/O itself (robots, pages and DNS all
// run in workers), so dispatching can never stall behind a slow host.

import (
	"context"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"cl0q/crawlutil"
	"cl0q/dbresil"
)

// item is one URL waiting in the BFS frontier.
type item struct {
	url   string
	depth int
}

// jobKind distinguishes the two kinds of fetch work.
type jobKind int

const (
	jobRobots jobKind = iota // fetch + parse one host's robots.txt (paced like any request to that host)
	jobPage                  // fetch one page (its host's robots verdict is already cached)
)

// fetchJob is the unit of concurrent work handed to a worker.
type fetchJob struct {
	kind      jobKind
	url       string
	depth     int
	host      string
	rules     *robotRules   // page jobs only: the host's cached verdict (non-nil)
	gap       time.Duration // required gap after this request: hostDelay, or the Crawl-delay if longer
	crawlSubs bool          // cycle's crawl_subdomains setting (apex folding)
	maxDepth  int           // BFS depth bound
}

// crawlCandidate is a link a worker extracted that may join the BFS frontier.
// The dispatcher applies the shared-state filters (visited, per-host queue
// cap, queue-cap) before enqueueing.
type crawlCandidate struct {
	url   string
	depth int
	dom   string // folded registrable domain, for the per-host queue cap
	host  string // the candidate URL's hostname, for hostQueued accounting
}

// pageOutcome is what a worker returns for a page job: resolved new-domain
// candidates (DNS already done, in parallel across workers) and BFS
// candidates. The dispatcher dedups domains via seenDomain before buffering.
type pageOutcome struct {
	domains    []pendDomain
	candidates []crawlCandidate
}

// fetchResult is one completed job, stamped with when its request finished —
// the dispatcher uses doneAt as the host's lastFetch, exactly like the old
// sequential loop stamped hostLast after each fetch.
type fetchResult struct {
	job     fetchJob
	doneAt  time.Time
	robots  *robotRules  // robots jobs only (non-nil)
	outcome *pageOutcome // page jobs only (non-nil)
}

// hostState is one host's politeness state for the cycle. Only the
// dispatcher's goroutine reads or writes it — no mutex.
type hostState struct {
	lastFetch time.Time   // when the host's last request (robots or page) completed
	pages     int         // pages fetched this cycle (cap: perHostCap)
	inFlight  bool        // a worker currently holds a job for this host
	robots    *robotRules // nil until a robots job for this host completes
}

// dispatchConfig wires the dispatcher to the crawler's knobs. hostDelay and
// perHostCap carry the ALREADY-VALIDATED values (floors enforced in
// validateConfig): the dispatcher only ever makes pacing gentler.
type dispatchConfig struct {
	workers    int           // concurrent fetch workers (DISCOVERY_WORKERS, 1..maxDiscoveryWorkers)
	hostDelay  time.Duration // ≥2s between requests to one host
	perHostCap int           // ≤25 pages per host per cycle
	batchSize  int           // discovered domains buffered per INSERT
	queueCap   int           // max URLs queued at once (0=unbounded)
	maxDepth   int           // BFS depth bound
	maxPages   int           // max pages per cycle before re-seeding
	crawlSubs  bool          // follow subdomains (else fold to registrable apex)
}

// dispatcher is the per-cycle engine: frontier, politeness state, worker pool
// and result intake. All of its methods run on the cycle's single goroutine
// except the workers, which only touch their fetchJob/fetchResult.
type dispatcher struct {
	cfg     dispatchConfig
	fetch   func(fetchJob) fetchResult // the unit of concurrent work (injected for tests)
	flushFn func([]pendDomain) int     // inserts one buffer; returns new rows (injected for tests)

	jobs    chan fetchJob    // unbuffered: a submit blocks until a worker takes it (natural backpressure)
	results chan fetchResult // buffered: workers never block sending results
	wg      sync.WaitGroup

	hosts    map[string]*hostState
	inFlight int // jobs submitted but not yet completed

	queue      []item
	visited    map[string]bool
	seenDomain map[string]bool
	hostQueued map[string]int

	buf []pendDomain

	pages         int // pages fetched this cycle
	discovered    int // unique new domains seen this cycle
	inserted      int // new rows written this cycle
	robotsFetched int // hosts whose robots.txt was fetched this cycle
	capped        bool
}

// newDispatcher builds a dispatcher for one BFS cycle.
func newDispatcher(cfg dispatchConfig, fetch func(fetchJob) fetchResult, flushFn func([]pendDomain) int) *dispatcher {
	return &dispatcher{
		cfg:        cfg,
		fetch:      fetch,
		flushFn:    flushFn,
		jobs:       make(chan fetchJob),                   // unbuffered: backpressure, no pile-up
		results:    make(chan fetchResult, 4*cfg.workers), // workers never block on send (inFlight ≤ workers)
		hosts:      map[string]*hostState{},
		visited:    map[string]bool{},
		seenDomain: map[string]bool{},
		hostQueued: map[string]int{},
	}
}

// start launches the worker pool.
func (d *dispatcher) start() {
	for i := 0; i < d.cfg.workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for job := range d.jobs {
				d.results <- d.fetch(job)
			}
		}()
	}
}

// seed populates the frontier for a cycle (fixed seeds + sampled domains),
// with the same apex-only filtering the sequential loop applied.
func (d *dispatcher) seed(seeds []string) {
	for _, s := range seeds {
		if d.visited[s] {
			continue
		}
		if !d.cfg.crawlSubs {
			if u, err := url.Parse(s); err == nil && crawlutil.IsSubdomain(u.Hostname()) {
				continue
			}
		}
		d.visited[s] = true
		d.queue = append(d.queue, item{s, 0})
	}
}

// stateFor returns (creating) the politeness state for a host.
func (d *dispatcher) stateFor(host string) *hostState {
	st, ok := d.hosts[host]
	if !ok {
		st = &hostState{}
		d.hosts[host] = st
	}
	return st
}

// gapFor returns the required inter-request gap for a host whose robots
// verdict is known (hostDelay, or the host's Crawl-delay if longer).
func (d *dispatcher) gapFor(st *hostState) time.Duration {
	gap := d.cfg.hostDelay
	if st.robots != nil && st.robots.Delay() > gap {
		gap = st.robots.Delay()
	}
	return gap
}

// submit hands a job to a worker and reserves the host: no other job for
// this host can be dispatched until this one completes. Unbuffered jobs
// channel: submit blocks while every worker is busy (backpressure).
func (d *dispatcher) submit(job fetchJob) {
	d.stateFor(job.host).inFlight = true
	d.inFlight++
	d.jobs <- job
}

// fill scans the frontier once and dispatches every eligible item: a robots
// job for a host whose verdict is unknown (at most one per host per cycle —
// the inFlight flag is the singleflight guard), or a page job for a host
// whose gap has elapsed and whose cycle cap isn't hit. Not-yet-due items
// stay queued; capped/blocked/never-fetch items are dropped, exactly like
// the old loop's `continue` paths. Returns the number of jobs dispatched.
func (d *dispatcher) fill() int {
	dispatched := 0
	kept := d.queue[:0]
	for _, cur := range d.queue {
		cu, err := url.Parse(cur.url)
		if err != nil || cu.Hostname() == "" {
			continue // drop: unparseable, like the old loop
		}
		host := strings.ToLower(cu.Hostname())
		st := d.stateFor(host)
		if st.pages >= d.cfg.perHostCap {
			continue // drop: this host hit its per-cycle page cap
		}
		if cur.depth > 0 && skipURL(cu) {
			continue // drop: never-fetch URL
		}
		if st.robots == nil {
			if st.inFlight {
				kept = append(kept, cur) // a robots job for this host is already out
				continue
			}
			if wait := d.cfg.hostDelay - time.Since(st.lastFetch); wait > 0 {
				kept = append(kept, cur) // not due yet
				continue
			}
			d.submit(fetchJob{kind: jobRobots, url: cur.url, depth: cur.depth, host: host,
				gap: d.cfg.hostDelay, crawlSubs: d.cfg.crawlSubs, maxDepth: d.cfg.maxDepth})
			dispatched++
			continue
		}
		if st.robots.blocked || !st.robots.allows(cu) {
			continue // drop: robots refuses us
		}
		if st.inFlight {
			kept = append(kept, cur) // same host is serialized: one in-flight job at a time
			continue
		}
		if wait := d.gapFor(st) - time.Since(st.lastFetch); wait > 0 {
			kept = append(kept, cur) // not due yet
			continue
		}
		d.submit(fetchJob{kind: jobPage, url: cur.url, depth: cur.depth, host: host,
			rules: st.robots, gap: d.gapFor(st), crawlSubs: d.cfg.crawlSubs, maxDepth: d.cfg.maxDepth})
		dispatched++
	}
	d.queue = kept
	return dispatched
}

// poll drains one completed result without blocking.
func (d *dispatcher) poll() (fetchResult, bool) {
	select {
	case res := <-d.results:
		return res, true
	default:
		return fetchResult{}, false
	}
}

// processResult folds one completed job back into cycle state: stamps the
// host's lastFetch (the request's completion time), clears the in-flight
// reservation, and handles the outcome. Runs on the dispatcher's goroutine.
func (d *dispatcher) processResult(res fetchResult) {
	st := d.stateFor(res.job.host)
	st.inFlight = false
	st.lastFetch = res.doneAt
	d.inFlight--

	if res.job.kind == jobRobots {
		d.robotsFetched++
		st.robots = res.robots
		if res.robots.blocked {
			return // drop: the site refused the robots request — skip it this cycle
		}
		u, err := url.Parse(res.job.url)
		if err != nil || !res.robots.allows(u) {
			return // drop: robots disallows the page
		}
		// First contact done: the page waits behind the gap like any other
		// request — re-queue at the back, exactly like the old loop.
		d.queue = append(d.queue, item{url: res.job.url, depth: res.job.depth})
		return
	}

	st.pages++
	d.pages++
	if d.pages%50 == 0 {
		log.Printf("[INFO] pages=%d queue=%d domains_seen=%d new_inserted=%d robots_checked=%d",
			d.pages, len(d.queue), d.discovered, d.inserted, d.robotsFetched)
	}
	if d.pages >= d.cfg.maxPages {
		d.capped = true
	}
	for _, pd := range res.outcome.domains {
		if d.seenDomain[pd.domain] {
			continue
		}
		d.seenDomain[pd.domain] = true
		d.discovered++
		d.buf = append(d.buf, pd)
		if len(d.buf) >= d.cfg.batchSize {
			d.flush()
		}
	}
	for _, c := range res.outcome.candidates {
		if d.visited[c.url] {
			continue
		}
		if d.hostQueued[c.dom] >= d.cfg.perHostCap {
			continue // don't fill the queue with a capped site
		}
		if d.cfg.queueCap > 0 && len(d.queue) >= d.cfg.queueCap {
			continue // bounded enqueue: a fan-out burst can't OOM the box
		}
		d.visited[c.url] = true
		d.hostQueued[c.host]++
		d.queue = append(d.queue, item{c.url, c.depth})
	}
}

// flush inserts the buffered domains and resets the buffer. Discovered
// domains are never dropped: flush runs on batch-full, at cycle end, and
// after shutdown drains in-flight results.
func (d *dispatcher) flush() {
	if len(d.buf) == 0 {
		return
	}
	d.inserted += d.flushFn(d.buf)
	d.buf = d.buf[:0]
}

// earliestWait returns the shortest time until any queued URL becomes
// eligible. After fill() dispatched nothing and dropped everything
// undispatchable, every remaining URL is gap-waiting, so this is positive;
// ok=false means nothing is waiting (defensive: never busy-spin).
func (d *dispatcher) earliestWait() (wait time.Duration, ok bool) {
	for _, cur := range d.queue {
		cu, err := url.Parse(cur.url)
		if err != nil || cu.Hostname() == "" {
			continue
		}
		st := d.stateFor(strings.ToLower(cu.Hostname()))
		gap := d.cfg.hostDelay
		if st.robots != nil && st.robots.Delay() > gap {
			gap = st.robots.Delay()
		}
		if w := gap - time.Since(st.lastFetch); w > 0 {
			if !ok || w < wait {
				wait, ok = w, true
			}
		}
	}
	return wait, ok
}

// drained reports whether the cycle has no more work: frontier empty and no
// job in flight.
func (d *dispatcher) drained() bool {
	return len(d.queue) == 0 && d.inFlight == 0
}

// pump runs one dispatcher round: drain completed results, dispatch eligible
// work, and wait when workers are busy but nothing new is eligible yet.
// Returns false when the cycle is drained, capped, or the context ended.
func (d *dispatcher) pump(ctx context.Context) bool {
	for {
		res, ok := d.poll()
		if !ok {
			break
		}
		d.processResult(res)
	}
	if d.capped {
		return false
	}
	if d.fill() > 0 {
		return true
	}
	if d.capped {
		return false
	}
	if d.inFlight > 0 {
		// Workers are busy but nothing new is eligible: wait for the next
		// completion — it frees its host and may unblock dispatch. Bounded
		// by the fetch timeouts, so this can't hang.
		select {
		case res := <-d.results:
			d.processResult(res)
			return !d.capped
		case <-ctx.Done():
			return false
		}
	}
	if d.drained() {
		return false
	}
	// Nothing eligible and nothing in flight: every remaining URL is waiting
	// out its host's gap. Sleep the shortest wait (the old SleepCtx path).
	if wait, ok := d.earliestWait(); ok {
		return dbresil.SleepCtx(ctx, wait)
	}
	return false
}

// shutdown stops dispatching, lets in-flight jobs finish ("stop after the
// page in flight"), folds their results into cycle state, and flushes. Any
// domains discovered by in-flight work are still written — never lost.
func (d *dispatcher) shutdown() {
	close(d.jobs)
	d.wg.Wait()
	for {
		res, ok := d.poll()
		if !ok {
			break
		}
		d.processResult(res)
	}
	d.flush()
}

// buildOutcome runs a page job's link loop in the worker: extract links,
// fold subdomains to their registrable apex (unless crawl_subdomains), and
// DNS-resolve each new domain. DNS runs here — in parallel across workers —
// so the dispatcher never blocks on the network and keeps dispatching.
// Shared-state filters (seenDomain dedup, visited, per-host queue cap,
// queue-cap) stay in the dispatcher.
func buildOutcome(job fetchJob, links []string) *pageOutcome {
	oc := &pageOutcome{}
	seen := map[string]bool{}
	for _, raw := range links {
		dom, full := resolveLink(raw, job.url)
		reg := crawlutil.Registrable(dom)
		if reg == "" {
			continue // no link, an IP, or a bare public suffix (co.ke) — never a site
		}
		// Apex-only: FOLD a subdomain link to its registrable site (blog.acme.com → acme.com)
		// rather than dropping it — captures the parent apex (breadth) WITHOUT re-introducing
		// fan-out (one apex per family, deduped by seenDomain). full="" so we don't BFS the
		// subdomain URL itself; the apex is crawled by website-crawler.
		if !job.crawlSubs && crawlutil.IsSubdomain(dom) {
			dom = reg
			full = ""
		}
		if !seen[dom] {
			seen[dom] = true
			oc.domains = append(oc.domains, resolveDomain(dom, job.url))
		}
		if full != "" && job.depth < job.maxDepth {
			if fu, err := url.Parse(full); err == nil && !skipURL(fu) {
				oc.candidates = append(oc.candidates, crawlCandidate{
					url: full, depth: job.depth + 1, dom: dom, host: strings.ToLower(fu.Hostname()),
				})
			}
		}
	}
	return oc
}

// realFetch is the production unit of concurrent work: robots jobs fetch and
// parse robots.txt (paced like any request to the host); page jobs fetch the
// page and run the link loop. Every request goes through the shared
// httpClient, so netguard.Control still vets every connection.
func realFetch(job fetchJob) fetchResult {
	if job.kind == jobRobots {
		rules := fetchRobotsRules(job.host, job.gap)
		return fetchResult{job: job, doneAt: time.Now(), robots: rules}
	}
	links := safeCrawl(job.url, job.rules, job.gap)
	return fetchResult{job: job, doneAt: time.Now(), outcome: buildOutcome(job, links)}
}
