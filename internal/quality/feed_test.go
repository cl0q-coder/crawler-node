package quality

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubFeed is a scripted Feed for Refresher tests.
type stubFeed struct {
	name  string
	lines []string
	err   error
}

func (s *stubFeed) Name() string { return s.name }
func (s *stubFeed) Fetch(ctx context.Context) ([]string, error) {
	return s.lines, s.err
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"http://Evil-Example.COM/malware.php", "evil-example.com", true},
		{"https://sub.evil.com:8443/a?b=c", "sub.evil.com", true},
		{"WWW.Evil.COM", "evil.com", true},
		{"evil.com.", "evil.com", true},
		{"  evil.com  ", "evil.com", true},
		{"evil.com/path#frag", "evil.com", true},
		{"1.2.3.4", "1.2.3.4", true}, // IP literal: exact-host only
		{"http://1.2.3.4:8080/x", "1.2.3.4", true},
		{"xn--nxasmq6b.example", "xn--nxasmq6b.example", true}, // punycode passes through untouched
		{"# a comment", "", false},
		{"", "", false},
		{"not a host!!", "", false},
		{"nodot", "", false},                                 // single label without dot: rejected
		{"http://", "", false},                               // scheme with no host
		{"evil..com", "", false},                             // empty label
		{"-evil.com", "-evil.com", true},                     // leading hyphen: syntactically odd but harmless as exact-domain rule
		{"a." + strings.Repeat("b", 64) + ".com", "", false}, // label > 63
	}
	for _, c := range cases {
		got, ok := NormalizeHost(c.raw)
		if ok != c.ok || got != c.want {
			t.Errorf("NormalizeHost(%q) = (%q,%v), want (%q,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestRefresherInstallsAndDedupes(t *testing.T) {
	f := &stubFeed{name: "testfeed", lines: []string{
		"http://evil.com/x",
		"HTTP://EVIL.COM/y", // dup after normalization
		"# comment",
		"",
		"not a host!!", // dropped
		"phish.example.net",
	}}
	r := NewRefresher([]Feed{f}, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	doms := r.Domains()
	if len(doms) != 2 {
		t.Fatalf("want 2 domains, got %d: %v", len(doms), doms)
	}
	for h, prov := range doms {
		if prov != "testfeed" {
			t.Errorf("host %q provenance = %q, want testfeed", h, prov)
		}
	}
	if r.Count() != 2 {
		t.Errorf("Count = %d, want 2", r.Count())
	}
}

func TestRefresherKeepsPreviousOnFailure(t *testing.T) {
	f := &stubFeed{name: "flaky", lines: []string{"http://evil.com/"}}
	r := NewRefresher([]Feed{f}, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	// Now the feed fails: the previous set must survive.
	f.err = errors.New("boom")
	f.lines = nil
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatalf("want error from failing feed, got nil")
	}
	doms := r.Domains()
	if len(doms) != 1 || doms["evil.com"] != "flaky" {
		t.Fatalf("previous set not kept: %v", doms)
	}
	// And recovery works on the next good fetch.
	f.err = nil
	f.lines = []string{"http://new-evil.com/"}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("recovery Refresh: %v", err)
	}
	if doms := r.Domains(); len(doms) != 1 || doms["new-evil.com"] != "flaky" {
		t.Fatalf("after recovery want only new-evil.com, got %v", doms)
	}
}

func TestRefresherPartialFailureAcrossFeeds(t *testing.T) {
	good := &stubFeed{name: "good", lines: []string{"http://g-evil.com/"}}
	bad := &stubFeed{name: "bad", err: errors.New("down")}
	r := NewRefresher([]Feed{good, bad}, time.Hour)
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatalf("want aggregated error, got nil")
	}
	if doms := r.Domains(); len(doms) != 1 || doms["g-evil.com"] != "good" {
		t.Fatalf("good feed's hosts must install despite bad feed: %v", doms)
	}
	if st := r.FeedStatus()["bad"]; !strings.HasPrefix(st, "error:") {
		t.Errorf("bad feed status = %q, want error prefix", st)
	}
}

func TestRefresherEmptyFeedsDisabled(t *testing.T) {
	r := NewRefresher(nil, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("empty Refresh: %v", err)
	}
	if n := r.Count(); n != 0 {
		t.Fatalf("Count = %d, want 0", n)
	}
	if doms := r.Domains(); len(doms) != 0 {
		t.Fatalf("want empty snapshot, got %v", doms)
	}
}

// Feed output must be domain-kind only: no entry may look like a bare keyword
// that the matcher could substring against legit neighbors. Every normalized
// host is either a dotted domain or an IP literal.
func TestFeedOutputIsDomainKindOnly(t *testing.T) {
	f := &stubFeed{name: "t", lines: []string{
		"http://casino-evil.com/", "http://sub.phish.example/", "10.0.0.9",
		"evil", // rejected: no dot
	}}
	r := NewRefresher([]Feed{f}, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for h := range r.Domains() {
		if !strings.Contains(h, ".") {
			t.Errorf("feed host %q is not domain-kind (no dot)", h)
		}
	}
}

// Mutating the returned snapshot must not corrupt the refresher.
func TestDomainsSnapshotIsACopy(t *testing.T) {
	f := &stubFeed{name: "t", lines: []string{"http://evil.com/"}}
	r := NewRefresher([]Feed{f}, time.Hour)
	_ = r.Refresh(context.Background())
	snap := r.Domains()
	snap["injected.example"] = "t"
	if r.Count() != 1 {
		t.Fatalf("snapshot mutation leaked into refresher")
	}
}
