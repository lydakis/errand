# Grouped apply comparison

Individual complete-operation timings; A/B execute the same frozen baseline binary.

| Case | Baseline A ms | Baseline B ms | Candidate ms | Median paired C/A | Median paired C/B |
|---|---:|---:|---:|---:|---:|
| apply-1 | 127.687 | 118.344 | 123.321 | 0.958 | 0.995 |
| apply-8 | 445.363 | 441.250 | 193.274 | 0.434 | 0.438 |
| apply-32 | 1585.456 | 1580.591 | 463.128 | 0.292 | 0.293 |
| apply-128 | 5925.294 | 5870.326 | 1369.196 | 0.231 | 0.233 |
| apply-512 | 27046.908 | 27092.920 | 5098.513 | 0.187 | 0.187 |
| push | 455.050 | 454.598 | 442.373 | 0.974 | 0.967 |
| watch | 283.490 | 283.287 | 296.459 | 1.046 | 1.046 |
| fetch-batch | 6099.561 | 6269.108 | 1557.002 | 0.251 | 0.250 |
| workspace-create | 544.442 | 549.222 | 547.567 | 1.009 | 0.997 |
| ephemeral-job | 566.912 | 578.376 | 573.171 | 1.006 | 0.992 |
