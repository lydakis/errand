# Focused controls

Lower is faster. Ranges are observed extrema, not confidence intervals. Each case uses all six build orders twice. Baseline-a and baseline-b execute the same binary. Read each report for its timing budget.

## btrfs-controls

| Case | Comparison | Median paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| dispersed-1000 | baseline-b/baseline-a | 1.012 | 0.803–1.224 | 5/12 |
| dispersed-1000 | candidate/baseline-a | 0.640 | 0.511–0.759 | 12/12 |
| dispersed-1000 | candidate/baseline-b | 0.627 | 0.571–0.834 | 12/12 |
| fetch-false | baseline-b/baseline-a | 1.000 | 0.551–1.447 | 6/12 |
| fetch-false | candidate/baseline-a | 0.975 | 0.671–1.230 | 8/12 |
| fetch-false | candidate/baseline-b | 0.941 | 0.839–1.217 | 9/12 |
| mixed-1000 | baseline-b/baseline-a | 1.015 | 0.916–1.160 | 5/12 |
| mixed-1000 | candidate/baseline-a | 1.059 | 0.882–1.323 | 3/12 |
| mixed-1000 | candidate/baseline-b | 1.030 | 0.891–1.348 | 3/12 |
| mixed-10000 | baseline-b/baseline-a | 1.010 | 0.876–1.181 | 6/12 |
| mixed-10000 | candidate/baseline-a | 1.002 | 0.828–1.078 | 5/12 |
| mixed-10000 | candidate/baseline-b | 0.993 | 0.847–1.100 | 6/12 |

## btrfs-deferred

| Case | Comparison | Median paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| dispersed-1000 | candidate/baseline | 0.599 | 0.416–0.781 | 12/12 |
| dispersed-1000 | deferred/baseline | 0.691 | 0.459–0.902 | 12/12 |
| dispersed-1000 | deferred/candidate | 1.153 | 0.905–1.454 | 1/12 |
| mixed-1000 | candidate/baseline | 1.077 | 0.851–1.458 | 5/12 |
| mixed-1000 | deferred/baseline | 1.023 | 0.759–1.212 | 5/12 |
| mixed-1000 | deferred/candidate | 0.895 | 0.712–1.244 | 8/12 |
| mixed-10000 | candidate/baseline | 0.979 | 0.831–1.180 | 6/12 |
| mixed-10000 | deferred/baseline | 0.975 | 0.824–1.319 | 9/12 |
| mixed-10000 | deferred/candidate | 0.987 | 0.816–1.208 | 7/12 |

## btrfs-mismatch

| Case | Comparison | Median paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| dispersed-1000 | candidate/baseline | 0.631 | 0.424–0.756 | 12/12 |
| dispersed-1000 | mismatch/baseline | 0.574 | 0.474–0.758 | 12/12 |
| dispersed-1000 | mismatch/candidate | 0.932 | 0.713–1.257 | 9/12 |
| mixed-1000 | candidate/baseline | 1.071 | 0.608–1.196 | 4/12 |
| mixed-1000 | mismatch/baseline | 1.019 | 0.843–1.273 | 5/12 |
| mixed-1000 | mismatch/candidate | 1.021 | 0.772–1.387 | 6/12 |
| mixed-10000 | candidate/baseline | 0.984 | 0.894–1.068 | 7/12 |
| mixed-10000 | mismatch/baseline | 0.984 | 0.885–1.114 | 8/12 |
| mixed-10000 | mismatch/candidate | 1.011 | 0.905–1.098 | 6/12 |

## apfs-mismatch

| Case | Comparison | Median paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| dispersed-1000 | candidate/baseline | 0.595 | 0.582–0.659 | 12/12 |
| dispersed-1000 | mismatch/baseline | 0.595 | 0.580–0.609 | 12/12 |
| dispersed-1000 | mismatch/candidate | 1.000 | 0.896–1.027 | 6/12 |
| mixed-1000 | candidate/baseline | 0.999 | 0.972–1.038 | 6/12 |
| mixed-1000 | mismatch/baseline | 1.003 | 0.984–1.028 | 4/12 |
| mixed-1000 | mismatch/candidate | 1.006 | 0.973–1.027 | 4/12 |
| mixed-10000 | candidate/baseline | 1.001 | 0.994–1.008 | 4/12 |
| mixed-10000 | mismatch/baseline | 1.000 | 0.991–1.007 | 6/12 |
| mixed-10000 | mismatch/candidate | 1.001 | 0.987–1.008 | 6/12 |

