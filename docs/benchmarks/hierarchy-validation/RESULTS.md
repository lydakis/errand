# Shared hierarchy validation: native comparisons

Ratios are medians of matched candidate/baseline pairs, not ratios of the displayed medians. Lower is better. Ranges show observed pair extrema, not confidence intervals.

Update cases are 100-entry batches: 1K flat inventories and 10K indexed trees. Command benchmarks use loopback HTTP and exclude fixture setup; they do not measure cross-host delivery or shell startup. Journal history cases use 10K files; dense cases use 50K.

## APFS

Initial candidate: command matrix, crossed sparse histories and dense histories.

### Updates and complete commands

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| ephemeral-job | 528.184 | 527.770 | 0.995 | 0.993–1.013 | 4/6 |
| fetch-false | 99.636 | 99.678 | 1.003 | 0.946–1.033 | 3/6 |
| fetch-true | 197.782 | 199.412 | 1.006 | 0.988–1.033 | 2/6 |
| push | 482.210 | 480.133 | 1.000 | 0.917–1.005 | 3/6 |
| update-1000-dispersed (flat) | 0.071 | 0.042 | 0.595 | 0.576–0.619 | 6/6 |
| update-1000-mixed (flat) | 0.071 | 0.072 | 1.009 | 0.981–1.116 | 2/6 |
| update-1000-repeated (flat) | 0.058 | 0.032 | 0.557 | 0.529–0.575 | 6/6 |
| update-10000-dispersed (indexed) | 0.217 | 0.165 | 0.764 | 0.759–0.777 | 6/6 |
| update-10000-mixed (indexed) | 0.318 | 0.318 | 1.004 | 0.997–1.014 | 3/6 |
| update-10000-repeated (indexed) | 0.076 | 0.045 | 0.600 | 0.597–0.602 | 6/6 |
| watch | 312.050 | 311.389 | 1.006 | 0.881–1.260 | 2/6 |
| watch-burst | 3566.334 | 3580.005 | 1.032 | 0.910–1.103 | 2/6 |
| watch-structural | 404.141 | 403.077 | 1.000 | 0.899–1.009 | 3/6 |
| workspace-create | 534.446 | 531.825 | 0.997 | 0.980–1.000 | 5/6 |

### Complete preparation, including wire hashing

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint | 270.665 | 273.415 | 1.012 | 0.995–1.028 | 1/6 |
| dense-append / observation-journal | 350.562 | 322.316 | 0.926 | 0.891–0.940 | 6/6 |
| dense-append / observation-replacement | 272.544 | 275.327 | 1.008 | 0.981–1.026 | 1/6 |
| dense-compact / checkpoint | 636.961 | 616.987 | 0.966 | 0.956–1.034 | 5/6 |
| dense-compact / observation-journal | 735.269 | 685.181 | 0.928 | 0.918–0.940 | 6/6 |
| dense-compact / observation-replacement | 638.674 | 624.786 | 0.978 | 0.959–0.983 | 6/6 |
| dispersed-d0 / checkpoint | 77.874 | 78.269 | 1.006 | 0.669–1.070 | 13/36 |
| dispersed-d0 / observation-journal | 77.417 | 76.822 | 0.999 | 0.945–1.413 | 19/36 |
| dispersed-d0 / observation-replacement | 78.973 | 79.374 | 1.002 | 0.930–1.530 | 16/36 |
| dispersed-d31 / checkpoint | 78.609 | 78.726 | 1.003 | 0.932–1.092 | 18/36 |
| dispersed-d31 / observation-journal | 78.874 | 78.880 | 0.999 | 0.766–1.320 | 19/36 |
| dispersed-d31 / observation-replacement | 79.179 | 79.209 | 1.001 | 0.833–1.050 | 18/36 |
| dispersed-d8 / checkpoint | 78.501 | 78.447 | 0.989 | 0.788–1.093 | 22/36 |
| dispersed-d8 / observation-journal | 77.886 | 77.471 | 0.993 | 0.718–1.124 | 20/36 |
| dispersed-d8 / observation-replacement | 79.335 | 78.895 | 0.996 | 0.933–1.159 | 21/36 |
| repeated-d0 / checkpoint | 79.014 | 78.695 | 1.005 | 0.829–1.360 | 15/36 |
| repeated-d0 / observation-journal | 76.900 | 77.463 | 1.005 | 0.919–1.129 | 16/36 |
| repeated-d0 / observation-replacement | 79.500 | 79.102 | 0.998 | 0.748–1.113 | 19/36 |
| repeated-d31 / checkpoint | 78.782 | 78.517 | 0.999 | 0.809–1.114 | 20/36 |
| repeated-d31 / observation-journal | 78.231 | 77.640 | 1.004 | 0.772–1.113 | 15/36 |
| repeated-d31 / observation-replacement | 79.132 | 79.285 | 1.002 | 0.875–1.081 | 18/36 |
| repeated-d8 / checkpoint | 78.756 | 79.078 | 1.009 | 0.930–1.080 | 17/36 |
| repeated-d8 / observation-journal | 77.497 | 78.047 | 0.999 | 0.705–1.084 | 18/36 |
| repeated-d8 / observation-replacement | 79.448 | 78.981 | 0.985 | 0.937–1.189 | 21/36 |

### Dense preparation phases

Same samples and receipt checks as the complete preparation table. Phase savings are not a substitute for total cost.

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint / Load | 21.597 | 21.941 | 1.010 | 0.988–1.034 | 2/6 |
| dense-append / checkpoint / Index | 10.163 | 10.141 | 1.000 | 0.863–1.139 | 3/6 |
| dense-append / observation-journal / Load | 116.210 | 87.601 | 0.754 | 0.744–0.760 | 6/6 |
| dense-append / observation-journal / Index | 0.712 | 0.697 | 0.984 | 0.954–1.048 | 3/6 |
| dense-append / observation-replacement / Load | 24.588 | 24.003 | 0.973 | 0.901–1.089 | 4/6 |
| dense-append / observation-replacement / Index | 10.200 | 10.200 | 1.001 | 0.974–1.048 | 2/6 |
| dense-compact / checkpoint / Load | 21.596 | 21.837 | 1.007 | 0.981–1.021 | 2/6 |
| dense-compact / checkpoint / Index | 45.399 | 26.721 | 0.587 | 0.560–0.611 | 6/6 |
| dense-compact / observation-journal / Load | 116.687 | 86.918 | 0.747 | 0.736–0.757 | 6/6 |
| dense-compact / observation-journal / Index | 34.475 | 16.988 | 0.481 | 0.444–0.513 | 6/6 |
| dense-compact / observation-replacement / Load | 24.526 | 24.608 | 0.998 | 0.983–1.030 | 3/6 |
| dense-compact / observation-replacement / Index | 45.242 | 26.337 | 0.583 | 0.573–0.598 | 6/6 |

## BTRFS

Initial candidate: command matrix, crossed sparse histories and dense histories.

### Updates and complete commands

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| ephemeral-job | 757.561 | 744.196 | 0.937 | 0.858–1.425 | 4/6 |
| fetch-false | 92.386 | 109.751 | 1.162 | 0.904–1.327 | 1/6 |
| fetch-true | 200.099 | 217.584 | 1.015 | 0.814–1.174 | 3/6 |
| push | 778.701 | 760.056 | 0.981 | 0.908–1.072 | 3/6 |
| update-1000-dispersed (flat) | 0.300 | 0.182 | 0.608 | 0.475–0.806 | 6/6 |
| update-1000-mixed (flat) | 0.273 | 0.323 | 1.138 | 1.030–1.454 | 0/6 |
| update-1000-repeated (flat) | 0.247 | 0.123 | 0.513 | 0.470–0.615 | 6/6 |
| update-10000-dispersed (indexed) | 1.150 | 0.998 | 0.843 | 0.799–1.124 | 5/6 |
| update-10000-mixed (indexed) | 1.698 | 1.741 | 1.058 | 0.952–1.286 | 2/6 |
| update-10000-repeated (indexed) | 0.343 | 0.231 | 0.694 | 0.571–0.765 | 6/6 |
| watch | 380.077 | 376.245 | 0.992 | 0.775–1.234 | 3/6 |
| watch-burst | 3009.581 | 3043.285 | 0.999 | 0.883–1.086 | 3/6 |
| watch-structural | 527.509 | 522.775 | 1.008 | 0.943–1.183 | 3/6 |
| workspace-create | 1811.090 | 1767.168 | 0.978 | 0.846–1.108 | 3/6 |

### Complete preparation, including wire hashing

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint | 645.042 | 618.243 | 0.954 | 0.854–1.051 | 4/6 |
| dense-append / observation-journal | 885.347 | 878.649 | 0.965 | 0.889–1.003 | 4/6 |
| dense-append / observation-replacement | 667.836 | 650.700 | 0.998 | 0.947–1.041 | 3/6 |
| dense-compact / checkpoint | 1065.440 | 1064.807 | 1.007 | 0.906–1.060 | 2/6 |
| dense-compact / observation-journal | 1410.858 | 1333.867 | 0.942 | 0.867–0.972 | 6/6 |
| dense-compact / observation-replacement | 1139.762 | 1093.428 | 0.971 | 0.827–1.051 | 4/6 |
| dispersed-d0 / checkpoint | 136.957 | 138.275 | 1.029 | 0.604–1.510 | 16/36 |
| dispersed-d0 / observation-journal | 126.369 | 127.297 | 1.006 | 0.848–1.205 | 17/36 |
| dispersed-d0 / observation-replacement | 140.136 | 141.605 | 1.004 | 0.853–1.178 | 17/36 |
| dispersed-d31 / checkpoint | 136.841 | 138.811 | 1.014 | 0.827–1.200 | 16/36 |
| dispersed-d31 / observation-journal | 131.654 | 131.248 | 0.998 | 0.841–1.156 | 18/36 |
| dispersed-d31 / observation-replacement | 146.034 | 141.071 | 0.990 | 0.855–1.254 | 19/36 |
| dispersed-d8 / checkpoint | 139.664 | 139.261 | 0.998 | 0.842–1.118 | 19/36 |
| dispersed-d8 / observation-journal | 127.130 | 131.216 | 1.034 | 0.864–1.321 | 11/36 |
| dispersed-d8 / observation-replacement | 140.519 | 141.898 | 1.002 | 0.873–1.144 | 17/36 |
| repeated-d0 / checkpoint | 135.448 | 135.287 | 0.996 | 0.828–4.361 | 19/36 |
| repeated-d0 / observation-journal | 127.477 | 127.963 | 1.003 | 0.862–1.436 | 18/36 |
| repeated-d0 / observation-replacement | 138.544 | 142.510 | 1.008 | 0.880–1.378 | 17/36 |
| repeated-d31 / checkpoint | 139.189 | 137.069 | 0.992 | 0.826–1.080 | 20/36 |
| repeated-d31 / observation-journal | 133.557 | 132.278 | 0.996 | 0.828–1.264 | 19/36 |
| repeated-d31 / observation-replacement | 137.807 | 142.079 | 1.012 | 0.819–1.312 | 17/36 |
| repeated-d8 / checkpoint | 135.525 | 135.084 | 0.999 | 0.871–1.132 | 18/36 |
| repeated-d8 / observation-journal | 130.301 | 134.392 | 1.021 | 0.826–1.173 | 14/36 |
| repeated-d8 / observation-replacement | 143.001 | 139.702 | 0.994 | 0.902–1.198 | 19/36 |

### Dense preparation phases

Same samples and receipt checks as the complete preparation table. Phase savings are not a substitute for total cost.

| Case | Baseline ms | Candidate ms | Paired ratio | Pair range | Wins |
|---|---:|---:|---:|---:|---:|
| dense-append / checkpoint / Load | 92.227 | 87.035 | 0.930 | 0.764–1.231 | 4/6 |
| dense-append / checkpoint / Index | 56.663 | 55.062 | 1.042 | 0.631–1.123 | 3/6 |
| dense-append / observation-journal / Load | 438.271 | 397.587 | 0.899 | 0.725–0.984 | 6/6 |
| dense-append / observation-journal / Index | 1.965 | 1.989 | 1.010 | 0.878–1.182 | 2/6 |
| dense-append / observation-replacement / Load | 108.276 | 108.451 | 0.984 | 0.825–1.103 | 4/6 |
| dense-append / observation-replacement / Index | 57.490 | 57.603 | 0.980 | 0.823–1.149 | 3/6 |
| dense-compact / checkpoint / Load | 82.968 | 88.644 | 1.091 | 0.790–1.421 | 2/6 |
| dense-compact / checkpoint / Index | 156.406 | 123.672 | 0.796 | 0.771–0.821 | 6/6 |
| dense-compact / observation-journal / Load | 442.025 | 387.296 | 0.872 | 0.812–0.911 | 6/6 |
| dense-compact / observation-journal / Index | 97.715 | 69.625 | 0.713 | 0.623–0.841 | 6/6 |
| dense-compact / observation-replacement / Load | 121.908 | 108.921 | 1.000 | 0.745–1.127 | 3/6 |
| dense-compact / observation-replacement / Index | 162.239 | 129.968 | 0.810 | 0.740–0.890 | 6/6 |

