package bench274;

import com.digitalpebble.stormcrawler.Constants;
import com.digitalpebble.stormcrawler.Metadata;
import com.digitalpebble.stormcrawler.bolt.FetcherBolt;
import com.digitalpebble.stormcrawler.bolt.JSoupParserBolt;
import com.digitalpebble.stormcrawler.bolt.URLPartitionerBolt;
import com.digitalpebble.stormcrawler.persistence.Status;
import com.digitalpebble.stormcrawler.spout.FileSpout;
import com.digitalpebble.stormcrawler.util.ConfUtils;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import org.apache.storm.Config;
import org.apache.storm.LocalCluster;
import org.apache.storm.topology.OutputFieldsDeclarer;
import org.apache.storm.topology.TopologyBuilder;
import org.apache.storm.topology.base.BaseRichBolt;
import org.apache.storm.task.OutputCollector;
import org.apache.storm.task.TopologyContext;
import org.apache.storm.tuple.Fields;
import org.apache.storm.tuple.Tuple;
import org.apache.storm.utils.Utils;

/**
 * Minimal StormCrawler 2.x driver for cl0q #274.
 *
 * <p>Local-mode topology: FileSpout -> URLPartitionerBolt -> FetcherBolt(N)
 * -> JSoupParserBolt -> CountingStatusBolt (parse default stream = one tuple
 * per parsed page; also catches fetch/parse errors from the status streams).
 * One page per seed host; exits (rc 0) when every seed has produced exactly
 * one terminal tuple.
 */
public class Bench274 {

    /**
     * Counts terminal outcomes; releases the latch when all seeds are done.
     *
     * <p>Storm serializes bolts at topology-creation time, so the shared
     * state is static (fine for local mode: everything runs in one JVM).
     *
     * <p>Subscribes to the parser's default stream (one tuple per parsed
     * page - has no "status" field, counts as FETCHED) and to the
     * fetch/parse status streams (errors, robots-denied, ...).
     */
    public static class CountingStatusBolt extends BaseRichBolt {
        private transient OutputCollector collector;
        private final int expected;
        static final AtomicInteger done = new AtomicInteger();
        static final AtomicInteger fetched = new AtomicInteger();
        static final AtomicInteger errors = new AtomicInteger();
        static volatile CountDownLatch latch;

        CountingStatusBolt(int expected) {
            this.expected = expected;
        }

        @Override
        public void prepare(Map<String, Object> conf, TopologyContext ctx, OutputCollector c) {
            collector = c;
        }

        @Override
        public void execute(Tuple t) {
            int n = done.incrementAndGet();
            Status s;
            try {
                s = (Status) t.getValueByField("status");
            } catch (IllegalArgumentException e) {
                // parser default stream: no status field means parsed OK
                s = Status.FETCHED;
            }
            if (s == Status.FETCHED) {
                fetched.incrementAndGet();
            } else {
                errors.incrementAndGet();
            }
            if (n % 100 == 0) {
                System.out.println("STATUS_PROGRESS " + n + "/" + expected);
            }
            collector.ack(t);
            CountDownLatch l = latch;
            if (n >= expected && l != null) {
                l.countDown();
            }
        }

        @Override
        public void declareOutputFields(OutputFieldsDeclarer declarer) {
            // terminal bolt: no output
        }
    }

    @SuppressWarnings({"unchecked", "rawtypes"})
    public static void main(String[] args) throws Exception {
        String seedsFile = args[0];
        int expected = Integer.parseInt(args[1]);
        int fetchers = Integer.parseInt(args[2]);

        Config conf = new Config();
        // StormCrawler defaults, same mechanism as ConfigurableTopology.start
        Map defaultSCConfig = Utils.findAndReadConfigFile("crawler-default.yaml", false);
        conf.putAll(ConfUtils.extractConfigElement(defaultSCConfig));

        // #274 apples-to-apples semantics: robots-first, 1s/host politeness
        // floor, 16 fetch workers, 1 page per host.
        conf.put("fetcher.server.delay", 1.0);
        conf.put("fetcher.server.min.delay", 0.0);
        conf.put("fetcher.threads.number", 1);
        conf.put("fetcher.threads.per.queue", 1);
        conf.put("fetcher.queue.mode", "byHost");
        // RFC 9309-compliant agent name: letters only (no digits - 2.11
        // hard-fails on anything else)
        conf.put("http.agent.name", "benchstorm");
        // okhttp impl skips cert validation by default (http.trust.everything=true),
        // keeping the TLS handshake real against the throwaway fixture CA
        conf.put(
                "http.protocol.implementation",
                "com.digitalpebble.stormcrawler.protocol.okhttp.HttpProtocol");
        conf.put(
                "https.protocol.implementation",
                "com.digitalpebble.stormcrawler.protocol.okhttp.HttpProtocol");
        conf.put("sitemap.discovery", false);
        // seeds-only bench: don't emit DISCOVERED for the fixture pages'
        // outlinks (no leg follows them)
        conf.put("parser.emitOutlinks", false);
        conf.put(Config.TOPOLOGY_WORKERS, 1);
        conf.put(Config.TOPOLOGY_DEBUG, false);

        Config.registerSerialization(conf, Metadata.class);
        Config.registerSerialization(conf, Status.class);

        TopologyBuilder builder = new TopologyBuilder();
        builder.setSpout("spout", new FileSpout(seedsFile), 1);
        builder.setBolt("partitioner", new URLPartitionerBolt(), 1).shuffleGrouping("spout");
        builder.setBolt("fetch", new FetcherBolt(), fetchers)
                .fieldsGrouping("partitioner", new Fields("key"));
        // Successful fetches go to the parser (default stream), not the
        // status stream — the parser reports the terminal FETCHED status.
        builder.setBolt("parse", new JSoupParserBolt(), 4).shuffleGrouping("fetch");

        CountDownLatch latch = new CountDownLatch(1);
        CountingStatusBolt counter = new CountingStatusBolt(expected);
        CountingStatusBolt.latch = latch;
        // parse default stream: one tuple per parsed page (counts as FETCHED);
        // status streams: fetch/parse errors, robots-denied, ...
        builder.setBolt("status", counter, 1)
                .shuffleGrouping("parse")
                .fieldsGrouping("parse", Constants.StatusStreamName, new Fields("url"))
                .fieldsGrouping("fetch", Constants.StatusStreamName, new Fields("url"));

        long t0 = System.nanoTime();
        LocalCluster cluster = new LocalCluster();
        boolean ok = false;
        try {
            cluster.submitTopology("bench274", conf, builder.createTopology());
            long tSubmit = System.nanoTime();

            ok = latch.await(600, TimeUnit.SECONDS);
            long t1 = System.nanoTime();
            System.out.printf(
                    "BENCH274_PHASE submit_s=%.2f crawl_s=%.2f done=%d/%d fetched=%d errors=%d complete=%b%n",
                    (tSubmit - t0) / 1e9,
                    (t1 - tSubmit) / 1e9,
                    CountingStatusBolt.done.get(),
                    expected,
                    CountingStatusBolt.fetched.get(),
                    CountingStatusBolt.errors.get(),
                    ok);
        } finally {
            cluster.shutdown();
        }
        System.exit(ok ? 0 : 1);
    }
}
