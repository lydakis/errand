# Shared hierarchy validation: native comparisons

Ratios are medians of matched candidate/baseline pairs, not ratios of the displayed medians. Lower is better. Ranges show observed pair extrema, not confidence intervals.

Update cases are 100-entry batches: 1K flat inventories and 10K indexed trees. Command benchmarks use loopback HTTP and exclude fixture setup; they do not measure cross-host delivery or shell startup. Journal history cases use 10K files; dense cases use 50K.

## APFS-FINAL

Original predicate candidate: command matrix and dense histories; sparse history is not repeated.

### Updates and complete commands

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| ephemeral-job | 526.722 | 524.177 | 0.997 | 0.993–1.005 | 5/6 |
| fetch-false | 99.168 | 98.906 | 0.994 | 0.975–1.020 | 4/6 |
| fetch-true | 197.478 | 197.164 | 1.003 | 0.986–1.018 | 3/6 |
| push | 482.437 | 480.790 | 0.996 | 0.991–1.002 | 5/6 |
| update-1000-dispersed (flat) | 0.071 | 0.042 | 0.590 | 0.580–0.608 | 6/6 |
| update-1000-mixed (flat) | 0.072 | 0.071 | 1.000 | 0.983–1.005 | 3/6 |
| update-1000-repeated (flat) | 0.059 | 0.032 | 0.546 | 0.530–0.556 | 6/6 |
| update-10000-dispersed (indexed) | 0.217 | 0.167 | 0.768 | 0.760–0.775 | 6/6 |
| update-10000-mixed (indexed) | 0.319 | 0.319 | 0.999 | 0.994–1.007 | 3/6 |
| update-10000-repeated (indexed) | 0.076 | 0.046 | 0.598 | 0.595–0.604 | 6/6 |
| watch | 313.105 | 309.923 | 0.980 | 0.785–1.007 | 4/6 |
| watch-burst | 3454.993 | 3525.284 | 1.038 | 0.907–1.071 | 2/6 |
| watch-structural | 400.358 | 400.172 | 1.001 | 0.981–1.014 | 3/6 |
| workspace-create | 526.501 | 527.418 | 1.002 | 0.990–1.014 | 2/6 |

### Complete preparation, including wire hashing

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint | 278.178 | 279.485 | 1.004 | 0.982–1.017 | 3/6 |
| dense-append / observation-journal | 362.000 | 333.262 | 0.917 | 0.907–0.929 | 6/6 |
| dense-append / observation-replacement | 279.941 | 279.762 | 1.000 | 0.971–1.026 | 3/6 |
| dense-compact / checkpoint | 651.721 | 632.657 | 0.969 | 0.959–1.000 | 6/6 |
| dense-compact / observation-journal | 752.840 | 707.300 | 0.942 | 0.921–0.945 | 6/6 |
| dense-compact / observation-replacement | 656.712 | 639.319 | 0.972 | 0.956–0.984 | 6/6 |

### Dense preparation phases

Same samples and receipt checks as the complete preparation table. Phase savings are not a substitute for total cost.

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint / Load | 22.445 | 22.303 | 0.990 | 0.967–1.007 | 4/6 |
| dense-append / checkpoint / Index | 10.537 | 10.547 | 0.995 | 0.978–1.107 | 3/6 |
| dense-append / observation-journal / Load | 120.015 | 90.046 | 0.750 | 0.730–0.761 | 6/6 |
| dense-append / observation-journal / Index | 0.727 | 0.718 | 0.999 | 0.971–1.020 | 3/6 |
| dense-append / observation-replacement / Load | 25.530 | 25.018 | 0.986 | 0.886–1.003 | 5/6 |
| dense-append / observation-replacement / Index | 9.905 | 10.324 | 0.990 | 0.865–1.138 | 4/6 |
| dense-compact / checkpoint / Load | 22.216 | 22.624 | 1.011 | 0.795–1.298 | 1/6 |
| dense-compact / checkpoint / Index | 46.488 | 27.166 | 0.583 | 0.577–0.597 | 6/6 |
| dense-compact / observation-journal / Load | 120.103 | 90.106 | 0.749 | 0.732–0.763 | 6/6 |
| dense-compact / observation-journal / Index | 37.068 | 16.000 | 0.445 | 0.417–0.499 | 6/6 |
| dense-compact / observation-replacement / Load | 25.571 | 24.970 | 0.983 | 0.943–1.056 | 4/6 |
| dense-compact / observation-replacement / Index | 46.405 | 27.223 | 0.589 | 0.552–0.592 | 6/6 |

## BTRFS-FINAL

Original predicate candidate: command matrix and dense histories; sparse history is not repeated.

### Updates and complete commands

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| ephemeral-job | 787.711 | 768.161 | 1.035 | 0.808–1.176 | 3/6 |
| fetch-false | 96.214 | 105.374 | 1.133 | 0.699–2.534 | 2/6 |
| fetch-true | 210.013 | 215.811 | 1.074 | 0.791–1.627 | 2/6 |
| push | 801.704 | 803.744 | 0.997 | 0.970–1.130 | 3/6 |
| update-1000-dispersed (flat) | 0.273 | 0.193 | 0.653 | 0.612–0.796 | 6/6 |
| update-1000-mixed (flat) | 0.310 | 0.290 | 0.939 | 0.818–1.115 | 4/6 |
| update-1000-repeated (flat) | 0.218 | 0.131 | 0.616 | 0.492–0.784 | 6/6 |
| update-10000-dispersed (indexed) | 1.099 | 0.909 | 0.828 | 0.796–0.925 | 6/6 |
| update-10000-mixed (indexed) | 1.738 | 1.635 | 0.946 | 0.888–1.096 | 4/6 |
| update-10000-repeated (indexed) | 0.388 | 0.226 | 0.615 | 0.543–0.718 | 6/6 |
| watch | 411.367 | 435.954 | 0.988 | 0.895–1.665 | 3/6 |
| watch-burst | 3033.237 | 3036.656 | 1.009 | 0.933–1.080 | 3/6 |
| watch-structural | 526.935 | 514.281 | 0.980 | 0.895–1.045 | 4/6 |
| workspace-create | 1729.833 | 1718.998 | 0.973 | 0.866–1.160 | 4/6 |

### Complete preparation, including wire hashing

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint | 665.397 | 635.162 | 0.945 | 0.880–1.015 | 4/6 |
| dense-append / observation-journal | 940.897 | 856.942 | 0.904 | 0.575–1.025 | 5/6 |
| dense-append / observation-replacement | 651.120 | 642.698 | 0.995 | 0.864–1.152 | 4/6 |
| dense-compact / checkpoint | 1106.855 | 1091.809 | 0.977 | 0.930–1.025 | 4/6 |
| dense-compact / observation-journal | 1472.631 | 1338.454 | 0.923 | 0.891–0.967 | 6/6 |
| dense-compact / observation-replacement | 1140.099 | 1095.239 | 0.941 | 0.897–1.072 | 5/6 |

### Dense preparation phases

Same samples and receipt checks as the complete preparation table. Phase savings are not a substitute for total cost.

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint / Load | 99.778 | 92.639 | 0.919 | 0.760–1.248 | 4/6 |
| dense-append / checkpoint / Index | 63.328 | 53.835 | 0.911 | 0.599–1.002 | 5/6 |
| dense-append / observation-journal / Load | 470.616 | 402.487 | 0.848 | 0.410–0.938 | 6/6 |
| dense-append / observation-journal / Index | 2.117 | 1.959 | 0.928 | 0.321–1.229 | 4/6 |
| dense-append / observation-replacement / Load | 105.200 | 100.913 | 0.964 | 0.867–1.047 | 4/6 |
| dense-append / observation-replacement / Index | 58.628 | 64.929 | 1.131 | 0.920–1.658 | 3/6 |
| dense-compact / checkpoint / Load | 107.081 | 86.733 | 0.807 | 0.770–1.025 | 4/6 |
| dense-compact / checkpoint / Index | 156.652 | 127.364 | 0.834 | 0.752–1.216 | 5/6 |
| dense-compact / observation-journal / Load | 456.442 | 389.193 | 0.847 | 0.820–0.901 | 6/6 |
| dense-compact / observation-journal / Index | 104.318 | 78.496 | 0.743 | 0.667–0.813 | 6/6 |
| dense-compact / observation-replacement / Load | 103.741 | 102.584 | 1.015 | 0.957–1.198 | 2/6 |
| dense-compact / observation-replacement / Index | 160.601 | 127.538 | 0.786 | 0.448–0.969 | 6/6 |

