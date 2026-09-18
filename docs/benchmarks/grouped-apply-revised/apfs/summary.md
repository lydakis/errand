# Grouped apply comparison

Individual complete-operation timings; A/B execute the same frozen baseline binary.

| Case | Baseline A ms | Baseline B ms | Candidate ms | Median paired C/A | Median paired C/B |
|---|---:|---:|---:|---:|---:|
| apply-1 | 119.748 | 123.680 | 121.861 | 0.960 | 0.989 |
| apply-8 | 464.352 | 466.156 | 187.226 | 0.405 | 0.403 |
| apply-32 | 1603.336 | 1627.390 | 480.837 | 0.300 | 0.295 |
| apply-128 | 5764.140 | 5927.454 | 1409.556 | 0.247 | 0.238 |
| apply-512 | 26900.755 | 26847.692 | 5332.640 | 0.200 | 0.199 |
| push | 455.087 | 461.020 | 446.971 | 0.976 | 0.994 |
| watch | 288.322 | 286.465 | 289.510 | 0.997 | 1.011 |
| fetch-batch | 5970.314 | 6212.316 | 1587.308 | 0.264 | 0.258 |
| workspace-create | 560.395 | 551.874 | 557.730 | 1.004 | 1.003 |
| ephemeral-job | 589.083 | 586.164 | 583.905 | 1.003 | 0.996 |
