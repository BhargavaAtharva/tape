# Verification

All outputs below were produced on an Apple M4, 16 GB, macOS, Go 1.27.4, Node 25, on 2026-11-15, by running the
commands shown. The machine was on battery in Low Power Mode with other workloads running; load averages are
recorded next to the timing runs.

## Static checks and tests

```
$ go vet ./... && gofmt -l .
vet/gofmt clean
ok  	github.com/BhargavaAtharva/tape/api	2.366s
ok  	github.com/BhargavaAtharva/tape/book	8.872s
?   	github.com/BhargavaAtharva/tape/cmd/loadgen	[no test files]
?   	github.com/BhargavaAtharva/tape/cmd/tape	[no test files]
ok  	github.com/BhargavaAtharva/tape/engine	12.504s
ok  	github.com/BhargavaAtharva/tape/feed	2.429s
ok  	github.com/BhargavaAtharva/tape/hist	3.690s
ok  	github.com/BhargavaAtharva/tape/sim	4.515s
ok  	github.com/BhargavaAtharva/tape/wal	6.236s
```

(`./...` at the time of that run also listed a Go package inside `web/node_modules`; `web/go.mod` now keeps it out.)

## Crash at a random point

Each trial SIGKILLs a child process running a durable engine at a random event, optionally damages the log tail
(`damage1` appends random garbage, `damage2` truncates 1 to 40 bytes), recovers, finishes the command stream and
compares the full event stream and final book hash with an uninterrupted run. SIGKILL keeps the page cache, so this
proves exact recovery of what reached the file under each policy; it does not simulate power loss.

```
$ go test -race -count=1 -v -run TestCrashAtRandomPoint ./engine
    recovery_test.go:302: published 8261 events before kill; snapshot 3083, replayed 768 commands in 3.09725ms
    recovery_test.go:302: published 32056 events before kill; snapshot 13328, replayed 1792 commands in 5.136417ms
    recovery_test.go:302: published 39660 events before kill; snapshot 17181, replayed 1536 commands in 5.131333ms
    recovery_test.go:302: published 15817 events before kill; snapshot 6151, replayed 1280 commands in 20.742834ms
    recovery_test.go:302: published 28834 events before kill; snapshot 12574, replayed 1024 commands in 12.111042ms
    recovery_test.go:302: published 3927 events before kill; snapshot 1564, replayed 512 commands in 5.306792ms
    recovery_test.go:302: published 28801 events before kill; snapshot 12559, replayed 1023 commands in 9.060875ms
    recovery_test.go:302: published 1206 events before kill; snapshot 0, replayed 804 commands in 5.808375ms
    recovery_test.go:302: published 4952 events before kill; snapshot 1541, replayed 1023 commands in 7.187375ms
    --- PASS: TestCrashAtRandomPoint/batch/damage0/kill@8235 (0.88s)
    --- PASS: TestCrashAtRandomPoint/interval/damage0/kill@31593 (0.97s)
    --- PASS: TestCrashAtRandomPoint/none/damage0/kill@39127 (0.63s)
    --- PASS: TestCrashAtRandomPoint/batch/damage1/kill@15451 (1.02s)
    --- PASS: TestCrashAtRandomPoint/interval/damage1/kill@28739 (0.85s)
    --- PASS: TestCrashAtRandomPoint/none/damage1/kill@3621 (0.86s)
    --- PASS: TestCrashAtRandomPoint/batch/damage2/kill@28783 (1.00s)
    --- PASS: TestCrashAtRandomPoint/interval/damage2/kill@1083 (0.53s)
    --- PASS: TestCrashAtRandomPoint/none/damage2/kill@4842 (0.63s)
ok  	github.com/BhargavaAtharva/tape/engine	8.599s
```

## Fuzzing (60 s)

```
$ go test -run '^$' -fuzz FuzzBook -fuzztime=60s ./book
fuzz: elapsed: 1m1s, execs: 1229804 (0/sec), new interesting: 33 (total: 189)
PASS
ok  	github.com/BhargavaAtharva/tape/book	61.495s
```

## Benchmarks

The same benchmark files run back to back at three commits, each checked out in a temporary worktree:
`944b375` (before the allocation work), `a529a8f` (after it), `HEAD` (then `38085a1`, the current matching and
engine code).

```
$ go test -run '^$' -bench . -benchtime=1s -count=5 ./book ./engine     # at each commit
pmset lowpowermode: 1, power: Now drawing from 'Battery Power'
### commit 944b375  loadavg { 7.50 4.98 4.05 }
BenchmarkApplyMixed-10       	 5897038	       202.3 ns/op	   4942568 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	 6049278	       204.7 ns/op	   4885866 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	 5895687	       205.0 ns/op	   4877828 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	 5770926	       205.2 ns/op	   4874437 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	 6046141	       204.2 ns/op	   4896683 orders/s	     435 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	 4780406	       262.3 ns/op	   3812241 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	 4645316	       222.2 ns/op	   4501143 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	 6417602	       230.0 ns/op	   4346921 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	 3979422	       354.0 ns/op	   2824630 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	 3649314	       290.3 ns/op	   3444770 orders/s	     328 B/op	       2 allocs/op
BenchmarkSweep-10            	 3454033	       322.2 ns/op	   3103463 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	 5296818	       261.4 ns/op	   3825577 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	 3435618	       346.3 ns/op	   2887478 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	 3500619	       325.9 ns/op	   3068284 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	 3315040	       348.7 ns/op	   2868005 orders/s	     816 B/op	       4 allocs/op
BenchmarkSubmitInMemory-10     	  870326	      1365 ns/op	    732374 orders/s	     952 B/op	       5 allocs/op
BenchmarkSubmitInMemory-10     	  942268	      1349 ns/op	    741539 orders/s	     952 B/op	       5 allocs/op
BenchmarkSubmitInMemory-10     	  919977	      1342 ns/op	    744918 orders/s	     951 B/op	       5 allocs/op
BenchmarkSubmitInMemory-10     	  843997	      1400 ns/op	    714096 orders/s	     953 B/op	       5 allocs/op
BenchmarkSubmitInMemory-10     	  906732	      1373 ns/op	    728581 orders/s	     954 B/op	       5 allocs/op
BenchmarkSubmitWALNoSync-10    	  651146	      1871 ns/op	    541899 orders/s	    1104 B/op	       5 allocs/op
BenchmarkSubmitWALNoSync-10    	  639481	      2005 ns/op	    506259 orders/s	    1091 B/op	       5 allocs/op
BenchmarkSubmitWALNoSync-10    	  522644	      2670 ns/op	    379631 orders/s	    1080 B/op	       5 allocs/op
BenchmarkSubmitWALNoSync-10    	  433780	      2479 ns/op	    409583 orders/s	    1059 B/op	       5 allocs/op
BenchmarkSubmitWALNoSync-10    	  541694	      3155 ns/op	    321816 orders/s	    1077 B/op	       5 allocs/op
BenchmarkRecoverReplay-10      	      27	  40564824 ns/op	   2465190 cmds/s	101889674 B/op	  237466 allocs/op
BenchmarkRecoverReplay-10      	      28	  37330860 ns/op	   2678749 cmds/s	101890115 B/op	  237466 allocs/op
BenchmarkRecoverReplay-10      	      39	  37496082 ns/op	   2666945 cmds/s	101891147 B/op	  237466 allocs/op
BenchmarkRecoverReplay-10      	      24	  44361894 ns/op	   2254187 cmds/s	101894550 B/op	  237468 allocs/op
BenchmarkRecoverReplay-10      	      30	  35255938 ns/op	   2836402 cmds/s	101888362 B/op	  237467 allocs/op
### commit a529a8f  loadavg { 6.58 5.07 4.13 }
BenchmarkApplyMixed-10       	 9315534	       115.2 ns/op	   8676831 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	10723228	       112.9 ns/op	   8859300 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	10578578	       113.3 ns/op	   8823554 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	10639944	       116.2 ns/op	   8605591 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	 9469874	       124.4 ns/op	   8039460 orders/s	       5 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	10433228	       112.6 ns/op	   8884519 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	10628493	       112.3 ns/op	   8905414 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	10689085	       113.0 ns/op	   8849089 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	10819102	       112.2 ns/op	   8915347 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	10633924	       113.0 ns/op	   8850741 orders/s	      56 B/op	       0 allocs/op
BenchmarkSweep-10            	11736902	       102.3 ns/op	   9777712 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	11690498	       102.3 ns/op	   9776333 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	11730314	       102.4 ns/op	   9768101 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	11741337	       102.2 ns/op	   9783575 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	11754712	       102.2 ns/op	   9783080 orders/s	       0 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  885529	      1270 ns/op	    787409 orders/s	     396 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  923508	      1234 ns/op	    810138 orders/s	     394 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  990541	      1240 ns/op	    806712 orders/s	     393 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  933355	      1275 ns/op	    784293 orders/s	     394 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  986233	      1227 ns/op	    814796 orders/s	     393 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  744808	      1694 ns/op	    600977 orders/s	     496 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  738043	      1686 ns/op	    602598 orders/s	     496 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  706209	      1720 ns/op	    591359 orders/s	     500 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  738122	      1634 ns/op	    622211 orders/s	     496 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  713083	      1699 ns/op	    598193 orders/s	     499 B/op	       0 allocs/op
BenchmarkRecoverReplay-10      	      58	  20541221 ns/op	   4868260 cmds/s	64679490 B/op	   11119 allocs/op
BenchmarkRecoverReplay-10      	      57	  20688883 ns/op	   4833514 cmds/s	64683193 B/op	   11120 allocs/op
BenchmarkRecoverReplay-10      	      58	  20697150 ns/op	   4831583 cmds/s	64679486 B/op	   11119 allocs/op
BenchmarkRecoverReplay-10      	      57	  20716778 ns/op	   4827005 cmds/s	64681903 B/op	   11120 allocs/op
BenchmarkRecoverReplay-10      	      58	  20934159 ns/op	   4776882 cmds/s	64681409 B/op	   11120 allocs/op
### commit HEAD  loadavg { 5.08 4.85 4.09 }
BenchmarkApplyMixed-10       	 8316699	       144.5 ns/op	   6921714 orders/s	       7 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	 7787344	       148.0 ns/op	   6755602 orders/s	       7 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	 8364267	       143.8 ns/op	   6952781 orders/s	       7 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	 8350180	       144.5 ns/op	   6920964 orders/s	       7 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	 8385081	       144.6 ns/op	   6917138 orders/s	       7 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	 7336356	       164.4 ns/op	   6082100 orders/s	      73 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	 7384160	       161.8 ns/op	   6179181 orders/s	      72 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	 7382977	       163.5 ns/op	   6116903 orders/s	      72 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	 7313124	       162.5 ns/op	   6153368 orders/s	      73 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	 7417628	       162.3 ns/op	   6163228 orders/s	      72 B/op	       0 allocs/op
BenchmarkSweep-10            	 9122757	       131.7 ns/op	   7595306 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	 9143224	       131.8 ns/op	   7584416 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	 9084614	       131.9 ns/op	   7583388 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	 9069960	       131.9 ns/op	   7583820 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	 9131750	       132.0 ns/op	   7575952 orders/s	       0 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 1000000	      1210 ns/op	    826569 orders/s	     374 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  895213	      1280 ns/op	    781470 orders/s	     373 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 1000000	      1270 ns/op	    787458 orders/s	     373 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  969634	      1226 ns/op	    815523 orders/s	     373 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	  976519	      1191 ns/op	    839296 orders/s	     373 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  751705	      1632 ns/op	    621544 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  732135	      1693 ns/op	    598488 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  751742	      1555 ns/op	    651066 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  772868	      1581 ns/op	    642051 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	  752516	      1874 ns/op	    538971 orders/s	     381 B/op	       0 allocs/op
BenchmarkRecoverReplay-10      	      60	  20584401 ns/op	   4858048 cmds/s	56188305 B/op	    5341 allocs/op
BenchmarkRecoverReplay-10      	      55	  22748019 ns/op	   4395987 cmds/s	56188314 B/op	    5341 allocs/op
BenchmarkRecoverReplay-10      	      64	  18836467 ns/op	   5308851 cmds/s	56188324 B/op	    5341 allocs/op
BenchmarkRecoverReplay-10      	      64	  18824169 ns/op	   5312320 cmds/s	56188315 B/op	    5341 allocs/op
BenchmarkRecoverReplay-10      	      63	  20751473 ns/op	   4818935 cmds/s	56188320 B/op	    5341 allocs/op
loadavg end { 3.88 4.53 4.01 }
```

## Load generator

In-process, WAL on (`go build -o bin/loadgen ./cmd/loadgen`):

```
lowpowermode=1 loadavg { 7.60 5.80 4.63 }
### ./bin/loadgen -mode=engine -sync=batch -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10s
commands=29545 engine_rejections=1392 failures=0 throughput=2954/s
latency p50=23.068671ms p99=31.457279ms p99.9=125.829119ms
### ./bin/loadgen -mode=engine -sync=batch -workers=256 -duration=10s
symbols=4 workers_per_symbol=256 duration=10.003s
commands=538330 engine_rejections=28930 failures=0 throughput=53819/s
latency p50=16.252927ms p99=39.845887ms p99.9=159.383551ms
### ./bin/loadgen -mode=engine -sync=interval -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10s
commands=72699 engine_rejections=4110 failures=0 throughput=7270/s
latency p50=10.485759ms p99=20.971519ms p99.9=26.214399ms
### ./bin/loadgen -mode=engine -sync=none -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10.001s
commands=3987479 engine_rejections=251446 failures=0 throughput=398714/s
latency p50=102.399µs p99=950.271µs p99.9=4.194303ms
### ./bin/loadgen -mode=engine -sync=batch -rate=2000 -duration=10s
symbols=4 open_loop_rate=2000/s duration=10s
commands=19970 engine_rejections=818 failures=0 throughput=1997/s
latency p50=13.631487ms p99=24.117247ms p99.9=27.262975ms
### ./bin/loadgen -mode=engine -sync=none -rate=100000 -duration=10s
symbols=4 open_loop_rate=100000/s duration=10s
commands=999996 engine_rejections=64569 failures=0 throughput=100000/s
latency p50=20.479µs p99=18.874367ms p99.9=35.651583ms
loadavg { 5.16 5.43 4.57 }
```

Over HTTP against the server binary (`go build -o bin/tape ./cmd/tape`, fresh data directory per run):

```
lowpowermode=1 loadavg { 5.13 5.41 4.57 }
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=batch ./bin/tape + ./bin/loadgen -mode=http -workers=32 -duration=10s
symbols=4 workers_per_symbol=32 duration=10.001s
commands=50568 engine_rejections=2346 failures=0 throughput=5056/s
latency p50=24.117247ms p99=54.525951ms p99.9=79.691775ms
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=batch ./bin/tape + ./bin/loadgen -mode=http -rate=2000 -duration=10s
symbols=4 open_loop_rate=2000/s duration=10s
commands=19983 engine_rejections=816 failures=0 throughput=1998/s
latency p50=15.204351ms p99=29.360127ms p99.9=46.137343ms
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=none ./bin/tape + ./bin/loadgen -mode=http -workers=32 -duration=10s
symbols=4 workers_per_symbol=32 duration=10.001s
commands=415036 engine_rejections=26929 failures=0 throughput=41499/s
latency p50=2.359295ms p99=12.582911ms p99.9=19.922943ms
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=none ./bin/tape + ./bin/loadgen -mode=http -rate=20000 -duration=10s
symbols=4 open_loop_rate=20000/s duration=10.001s
commands=199989 engine_rejections=12523 failures=0 throughput=19997/s
latency p50=245.759µs p99=18.874367ms p99.9=56.623103ms
loadavg { 9.22 6.40 4.97 }
```

## Container under Render's free-tier limits

```
$ docker build -t tape:local .
$ docker images tape:local --format '{{.Size}}'
15.9MB
$ docker run -d --name tape-test --memory=512m --cpus=0.1 -e PORT=10000 -p 10000:10000 tape:local
(time from docker run to the first 200 from /healthz, polled every 100 ms)
container start to healthy: 0.18 s
### docker run --memory=512m --cpus=0.1 -e PORT=10000 tape:local  (uptime: Up 4 hours)
### docker stats, 12 samples 5 s apart
3.57% 8.531MiB / 512MiB
3.02% 8.77MiB / 512MiB
3.88% 8.531MiB / 512MiB
3.87% 8.523MiB / 512MiB
1.43% 8.531MiB / 512MiB
2.16% 8.543MiB / 512MiB
2.23% 8.535MiB / 512MiB
1.65% 8.543MiB / 512MiB
3.13% 8.547MiB / 512MiB
3.12% 8.543MiB / 512MiB
2.84% 8.547MiB / 512MiB
2.14% 9.164MiB / 512MiB
### /v1/stats
orders_per_sec=216 commands=1560769 fills=123433 latency_ns={'p50': 1727, 'p99': 40959, 'p999': 57343}
### node web/scripts/smoke-ws.mjs http://localhost:10000 30
BTC-USD    updates=154 trades=116 resyncs=0 last_seq=853485 levels=63 ok
ETH-USD    updates=149 trades=83 resyncs=0 last_seq=851377 levels=84 ok
SOL-USD    updates=154 trades=95 resyncs=0 last_seq=853222 levels=75 ok
NIFTY      updates=162 trades=94 resyncs=0 last_seq=857271 levels=91 ok
### docker cp tape-test:/tmp/tape - | wc -c   (tar of the data dir after the run)
 25715712
```

The 30-second smoke run also shows that streams outlive the server's 10 s read and 15 s write timeouts, which the
WebSocket handler clears for its own connection.

## Bandwidth of one browser tab

`web/scripts/measure-tab.sh <repo> 120`: the server in memory with bots on, a byte-counting TCP proxy in front of
it, the Vite dev server pointed at the proxy, and the app open in headless Chrome. Counts are wire bytes, after
compression, including the initial page data.

```
# commit 3348464 (before the changes), default bot intensity 1
seconds=125.0 server_to_client_bytes=1361154 client_to_server_bytes=115990
server_to_client=10.63 KB/s (37.4 MB/h)

# current code, TAPE_SIM_INTENSITY=1
seconds=125.0 server_to_client_bytes=173507 client_to_server_bytes=22934
server_to_client=1.36 KB/s (4.8 MB/h)

# current code, default bot intensity 0.5
seconds=125.0 server_to_client_bytes=117647 client_to_server_bytes=21292
server_to_client=0.92 KB/s (3.2 MB/h)
```

## Frontend

```
$ cd web && npx tsc --noEmit && npm run lint && npm run build

✓ built in 196ms
```

`npm audit` reports a moderate advisory in `minimatch`, pulled in by the `typescript-eslint` lint tooling. It is a
development dependency and is not part of the built site.

## UI

`docs/screenshot.png` was captured by `web/scripts/screenshot.sh 40`: the server in memory with the bots on for 40
seconds, then the app rendered by headless Chrome at 1440x900. Earlier checks rendered the light theme and the
375px tabbed layout the same way (inside a 375px iframe, since headless Chrome will not make a window narrower than
500px).

## Not verified here

The service has not been deployed to Render or Vercel. The image was built and run locally with Render's memory and
CPU limits as above; Render's own network, proxy and sleep/wake behaviour were not exercised.
