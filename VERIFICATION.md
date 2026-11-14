# Verification

All outputs below were produced on Apple M4, 16 GB, macOS, Go 1.27.4, Node 25, on 2026-11-14, by running the
commands shown. Other builds were running on the same machine at the time.

## Static checks and full test suite

```
$ go vet ./... && gofmt -l .
(no output)

$ go test -race -count=1 ./...
ok  	github.com/BhargavaAtharva/tape/api	1.908s
ok  	github.com/BhargavaAtharva/tape/book	8.589s
?   	github.com/BhargavaAtharva/tape/cmd/loadgen	[no test files]
?   	github.com/BhargavaAtharva/tape/cmd/tape	[no test files]
ok  	github.com/BhargavaAtharva/tape/engine	8.514s
ok  	github.com/BhargavaAtharva/tape/feed	2.004s
ok  	github.com/BhargavaAtharva/tape/hist	4.106s
ok  	github.com/BhargavaAtharva/tape/sim	5.071s
ok  	github.com/BhargavaAtharva/tape/wal	4.573s
```

## Crash at a random point (milestone 5)

Each trial SIGKILLs a child process running a durable engine at a random event, optionally damages the log tail
(`damage1` appends random garbage, `damage2` truncates 1 to 40 bytes), recovers, finishes the command stream and
compares the full event stream and final book hash with an uninterrupted run.

```
$ go test -race -count=1 -v -run TestCrashAtRandomPoint ./engine
    recovery_test.go:302: published 2231 events before kill; snapshot 0, replayed 1297 commands in 3.220209ms
    recovery_test.go:302: published 14763 events before kill; snapshot 5147, replayed 1792 commands in 4.114375ms
    recovery_test.go:302: published 6099 events before kill; snapshot 1577, replayed 1536 commands in 3.359208ms
    recovery_test.go:302: published 35761 events before kill; snapshot 15367, replayed 1536 commands in 8.540625ms
    recovery_test.go:302: published 3885 events before kill; snapshot 0, replayed 1803 commands in 8.28875ms
    recovery_test.go:302: published 21797 events before kill; snapshot 9486, replayed 1024 commands in 6.221209ms
    recovery_test.go:302: published 18000 events before kill; snapshot 7702, replayed 1023 commands in 7.131333ms
    recovery_test.go:302: published 6624 events before kill; snapshot 1564, replayed 1535 commands in 6.670125ms
    recovery_test.go:302: published 26217 events before kill; snapshot 10787, replayed 1535 commands in 7.197791ms
    --- PASS: TestCrashAtRandomPoint/batch/damage0/kill@2083 (0.58s)
    --- PASS: TestCrashAtRandomPoint/interval/damage0/kill@14666 (0.51s)
    --- PASS: TestCrashAtRandomPoint/none/damage0/kill@5709 (0.52s)
    --- PASS: TestCrashAtRandomPoint/batch/damage1/kill@35371 (0.76s)
    --- PASS: TestCrashAtRandomPoint/interval/damage1/kill@3497 (0.55s)
    --- PASS: TestCrashAtRandomPoint/none/damage1/kill@21698 (0.49s)
    --- PASS: TestCrashAtRandomPoint/batch/damage2/kill@17931 (0.68s)
    --- PASS: TestCrashAtRandomPoint/interval/damage2/kill@6241 (0.54s)
    --- PASS: TestCrashAtRandomPoint/none/damage2/kill@25706 (0.49s)
PASS
ok  	github.com/BhargavaAtharva/tape/engine	6.375s
```

## Fuzzing (60 s, after the allocation changes)

```
$ go test -run '^$' -fuzz FuzzBook -fuzztime=60s ./book
fuzz: elapsed: 1m1s, execs: 1416082 (0/sec), new interesting: 26 (total: 152)
PASS
ok  	github.com/BhargavaAtharva/tape/book	61.546s
```

## Benchmarks (milestone 7)

Before: commit `944b375` checked out in a worktree with the current `book/bench_test.go` and the engine Submit
benchmarks copied in.

```
$ go test -run '^$' -bench . -benchtime=2s -count=5 ./book ./engine     # at 944b375
cpu: Apple M4
BenchmarkApplyMixed-10       	22262812	       104.2 ns/op	   9599561 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	22990441	       103.6 ns/op	   9653851 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	22670774	       103.4 ns/op	   9672948 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	23241925	       103.2 ns/op	   9687966 orders/s	     435 B/op	       2 allocs/op
BenchmarkApplyMixed-10       	23309947	       103.2 ns/op	   9693574 orders/s	     435 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	24637231	        97.49 ns/op	  10257983 orders/s	     327 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	24324786	        97.74 ns/op	  10231090 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	24626170	        97.73 ns/op	  10231974 orders/s	     327 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	24733034	        98.00 ns/op	  10203705 orders/s	     328 B/op	       2 allocs/op
BenchmarkRestAndCancel-10    	24516042	        97.68 ns/op	  10237406 orders/s	     328 B/op	       2 allocs/op
BenchmarkSweep-10            	20081329	       119.1 ns/op	   8394844 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	20328840	       119.2 ns/op	   8389803 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	20437555	       117.8 ns/op	   8491170 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	20321940	       118.1 ns/op	   8467536 orders/s	     816 B/op	       4 allocs/op
BenchmarkSweep-10            	20467824	       118.5 ns/op	   8435600 orders/s	     816 B/op	       4 allocs/op
cpu: Apple M4
BenchmarkSubmitInMemory-10     	 3373330	       933.8 ns/op	   1070891 orders/s	    1068 B/op	       4 allocs/op
BenchmarkSubmitInMemory-10     	 2553572	       917.8 ns/op	   1089517 orders/s	    1065 B/op	       4 allocs/op
BenchmarkSubmitInMemory-10     	 2581336	       934.7 ns/op	   1069909 orders/s	    1053 B/op	       4 allocs/op
BenchmarkSubmitInMemory-10     	 2519930	       989.6 ns/op	   1010556 orders/s	    1051 B/op	       4 allocs/op
BenchmarkSubmitInMemory-10     	 2324541	      1006 ns/op	    994362 orders/s	    1053 B/op	       4 allocs/op
BenchmarkSubmitWALNoSync-10    	 1926465	      1272 ns/op	    794536 orders/s	    1342 B/op	       4 allocs/op
BenchmarkSubmitWALNoSync-10    	 2003470	      1298 ns/op	    779587 orders/s	    1371 B/op	       4 allocs/op
BenchmarkSubmitWALNoSync-10    	 1967684	      1299 ns/op	    776976 orders/s	    1349 B/op	       4 allocs/op
BenchmarkSubmitWALNoSync-10    	 1900478	      1349 ns/op	    749545 orders/s	    1331 B/op	       4 allocs/op
BenchmarkSubmitWALNoSync-10    	 1904996	      1351 ns/op	    747130 orders/s	    1343 B/op	       4 allocs/op
```

After (current code):

```
$ go test -run '^$' -bench . -benchtime=2s -count=5 ./book ./engine
cpu: Apple M4
BenchmarkApplyMixed-10       	36962598	        58.67 ns/op	  17045663 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	41051605	        58.62 ns/op	  17057893 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	40770562	        58.52 ns/op	  17087097 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	41207110	        58.57 ns/op	  17074511 orders/s	       5 B/op	       0 allocs/op
BenchmarkApplyMixed-10       	41389508	        62.47 ns/op	  16008483 orders/s	       5 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	35287630	        72.14 ns/op	  13861496 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	33570247	        70.65 ns/op	  14154724 orders/s	      55 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	33727464	        69.91 ns/op	  14304562 orders/s	      56 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	34820730	        69.50 ns/op	  14387975 orders/s	      55 B/op	       0 allocs/op
BenchmarkRestAndCancel-10    	33613033	        96.89 ns/op	  10320793 orders/s	      55 B/op	       0 allocs/op
BenchmarkSweep-10            	33147796	        64.91 ns/op	  15406474 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	37770268	        63.31 ns/op	  15796480 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	37025394	        93.55 ns/op	  10689636 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	35482479	        66.66 ns/op	  15001448 orders/s	       0 B/op	       0 allocs/op
BenchmarkSweep-10            	39014133	        62.80 ns/op	  15923414 orders/s	       0 B/op	       0 allocs/op
cpu: Apple M4
BenchmarkSubmitInMemory-10     	 3096331	      1084 ns/op	    922719 orders/s	     383 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 2417517	      1007 ns/op	    993019 orders/s	     377 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 2452845	       943.5 ns/op	   1059908 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 2509174	       970.9 ns/op	   1029936 orders/s	     380 B/op	       0 allocs/op
BenchmarkSubmitInMemory-10     	 2532528	       947.7 ns/op	   1055192 orders/s	     381 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	 2019999	      1199 ns/op	    841924 orders/s	     673 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	 2109122	      1213 ns/op	    831635 orders/s	     685 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	 2049660	      1262 ns/op	    800305 orders/s	     665 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	 1893986	      1364 ns/op	    743054 orders/s	     655 B/op	       0 allocs/op
BenchmarkSubmitWALNoSync-10    	 1696377	      1214 ns/op	    832441 orders/s	     621 B/op	       0 allocs/op
BenchmarkRecoverReplay-10      	     176	  15058828 ns/op	   6640623 cmds/s	64679399 B/op	   11119 allocs/op
BenchmarkRecoverReplay-10      	     163	  13925288 ns/op	   7181180 cmds/s	64677666 B/op	   11119 allocs/op
BenchmarkRecoverReplay-10      	     193	  12473919 ns/op	   8016726 cmds/s	64679946 B/op	   11120 allocs/op
BenchmarkRecoverReplay-10      	     183	  12180375 ns/op	   8209928 cmds/s	64681671 B/op	   11120 allocs/op
BenchmarkRecoverReplay-10      	     219	  11165029 ns/op	   8956537 cmds/s	64679448 B/op	   11120 allocs/op
```

## Load generator

In-process, WAL on (`go build -o bin/loadgen ./cmd/loadgen`):

```
### ./bin/loadgen -mode=engine -sync=batch -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10s
commands=29293 failures=0 throughput=2929/s
latency p50=23.068671ms p99=35.651583ms p99.9=44.040191ms
### ./bin/loadgen -mode=engine -sync=batch -workers=256 -duration=10s
symbols=4 workers_per_symbol=256 duration=10.002s
commands=498055 failures=0 throughput=49795/s
latency p50=18.874367ms p99=41.943039ms p99.9=71.303167ms
### ./bin/loadgen -mode=engine -sync=interval -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10.002s
commands=77100 failures=0 throughput=7708/s
latency p50=10.485759ms p99=22.020095ms p99.9=30.408703ms
### ./bin/loadgen -mode=engine -sync=none -workers=16 -duration=10s
symbols=4 workers_per_symbol=16 duration=10s
commands=6627726 failures=0 throughput=662770/s
latency p50=53.247µs p99=311.295µs p99.9=5.767167ms
```

Over HTTP against the server binary (`go build -o bin/tape ./cmd/tape`, fresh data dir per run):

```
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=batch ./bin/tape  +  ./bin/loadgen -mode=http -workers=32 -duration=10s
symbols=4 workers_per_symbol=32 duration=10.001s
commands=49678 failures=0 throughput=4967/s
latency p50=24.117247ms p99=56.623103ms p99.9=75.497471ms
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=interval ./bin/tape  +  ./bin/loadgen -mode=http -workers=32 -duration=10s
symbols=4 workers_per_symbol=32 duration=10.001s
commands=132984 failures=0 throughput=13297/s
latency p50=7.077887ms p99=50.331647ms p99.9=65.011711ms
### TAPE_SIM=false TAPE_RATE_LIMIT=0 TAPE_WAL_SYNC=none ./bin/tape  +  ./bin/loadgen -mode=http -workers=32 -duration=10s
symbols=4 workers_per_symbol=32 duration=10.002s
commands=866008 failures=0 throughput=86586/s
latency p50=1.179647ms p99=6.291455ms p99.9=11.534335ms
```

## Frontend build

```
$ cd web && npx tsc --noEmit && npm run build
(tsc: no output)
dist/assets/index-hI8utu5q.js                                   247.84 kB │ gzip: 78.15 kB

✓ built in 110ms
```

## Live server smoke test

Server started with `TAPE_DATA_DIR=/tmp/tape-final ./bin/tape` (bots on), then hit with curl and the WebSocket
smoke script, which checks snapshot then gap-free `prev`/`seq` chaining, that the locally maintained book never
crosses, and that it equals a second client's fresh snapshot at the same sequence number. The server was then
stopped with SIGTERM and restarted on the same directory with the bots off; the resting order placed earlier is
still there.

```
### curl
ok
[{"symbol":"BTC-USD","price_scale":2,"last":6422942,"open":6425906,"high":6433954,"low":6419258,"volume":152,"trades":23,"best_bid":6419258,"best_ask":6422942},{"symbol":"ETH-USD","price_scale":2,"last":312626,"open":312101,"high":312808,"low":312008,"volume":142,"trades":26,"best_bid":312626,"best_
{"order_id":2199023255636,"symbol":"ETH-USD","status":"resting","filled":0,"remaining":2,"events":[{"seq":731,"time":1794664525296,"kind":"accepted","side":"buy","order_id":2199023255636,"price":10000
[{"order_id":2199023255636,"side":"buy","price":100000,"qty":2,"post_only":false}]

{"at":1794664524334,"window_ms":999,"orders_per_sec":298.0003349523765,"commands":1146,"fills":82,"latency_ns":{"p50":10485759,"p99":13107199,"p999":14155775},"symbols":[{"symbol":"BTC-USD","orders_per_sec":81.00009104410233,"commands":343,"fills":20,"seq":900,"queue_depth":0,"latency_ns":{"p50":10485759,"p99":12058623,"p999":12058623},"wal":{"durable":true,"written":343,"synced":343,"lag":0,"snap
### node scripts/smoke-ws.mjs http://localhost:8081 6
BTC-USD    updates=242 trades=44 last_seq=1839 levels=54 ok
ETH-USD    updates=304 trades=35 last_seq=1788 levels=55 ok
SOL-USD    updates=227 trades=34 last_seq=1864 levels=48 ok
NIFTY      updates=363 trades=37 last_seq=1975 levels=65 ok
### restart after SIGTERM
time=2026-11-14T19:25:38.504+05:30 level=INFO msg=recovered symbol=BTC-USD snapshot=1142 replayed=0 took=241.667µs
time=2026-11-14T19:25:38.504+05:30 level=INFO msg=recovered symbol=ETH-USD snapshot=1117 replayed=0 took=190.417µs
time=2026-11-14T19:25:38.504+05:30 level=INFO msg=recovered symbol=SOL-USD snapshot=1109 replayed=0 took=169.208µs
time=2026-11-14T19:25:38.504+05:30 level=INFO msg=recovered symbol=NIFTY snapshot=1125 replayed=0 took=168.208µs
time=2026-11-14T19:25:38.504+05:30 level=INFO msg=listening addr=:8081 symbols=4 wal=batch data=/tmp/tape-final sim=false
[{"order_id":2199023255636,"side":"buy","price":100000,"qty":2,"post_only":false}]

```

## UI check

`npm run dev` against the running server, rendered with headless Chrome at 1440x900 (dark), 1280x800 (light, via
`prefers-color-scheme`) and at 375px width inside an iframe (headless Chrome will not make a window narrower than
500px). Book ladder with depth bars and change flashes, candlestick chart, order entry, trades tape and engine stats
all populated from live data; the narrow layout switches to tabs.

## Not verified here

The Docker image was not built: Docker is not installed on this machine. The same static Linux binary the
Dockerfile builds was produced with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/tape` (ELF 64-bit,
statically linked), and the server was run locally with the container's settings (`PORT=7860
TAPE_WAL_SYNC=interval TAPE_TRUST_PROXY=true`) and answered `/healthz` and a CORS request.
