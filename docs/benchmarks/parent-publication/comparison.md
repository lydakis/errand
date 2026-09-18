Durations are individual-operation medians. Reductions are medians of same-round ratios, not ratios of the displayed medians. Faster pairs count individual rounds.

| Host / workload | Baseline A / B | Candidate | Paired reduction A / B | Faster pairs A / B |
|---|---:|---:|---:|---:|
| APFS / tiny | 102.38 / 103.29 ms | 103.32 ms | -0.5% / -0.3% | 11/24 / 10/24 |
| APFS / flat128 | 873.00 / 874.31 ms | 866.64 ms | 0.6% / 0.7% | 9/12 / 9/12 |
| APFS / parents8 | 953.45 / 956.69 ms | 925.79 ms | 3.3% / 3.2% | 11/12 / 11/12 |
| APFS / parents128 | 1499.83 / 1498.32 ms | 1001.09 ms | 33.2% / 33.1% | 12/12 / 12/12 |
| BTRFS / tiny | 78.36 / 79.71 ms | 77.80 ms | -1.6% / 3.6% | 11/24 / 15/24 |
| BTRFS / flat128 | 2462.43 / 2644.53 ms | 2734.00 ms | -7.2% / -0.6% | 3/12 / 6/12 |
| BTRFS / parents8 | 2541.43 / 2538.76 ms | 2732.22 ms | 0.0% / -3.9% | 6/12 / 4/12 |
| BTRFS / parents128 | 3315.76 / 3004.31 ms | 2963.83 ms | 3.6% / -0.9% | 7/12 / 6/12 |
