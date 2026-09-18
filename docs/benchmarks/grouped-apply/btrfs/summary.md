# Grouped apply comparison

Individual complete-operation timings; A/B execute the same frozen baseline binary.

| Case | Baseline A ms | Baseline B ms | Candidate ms | Median paired C/A | Median paired C/B |
|---|---:|---:|---:|---:|---:|
| apply-1 | 122.766 | 125.666 | 119.420 | 0.995 | 0.976 |
| apply-8 | 398.344 | 376.502 | 246.869 | 0.621 | 0.647 |
| apply-32 | 1518.171 | 1529.363 | 747.295 | 0.537 | 0.481 |
| apply-128 | 5100.672 | 5422.123 | 2582.475 | 0.503 | 0.504 |
| apply-512 | 24684.706 | 24883.400 | 11075.851 | 0.456 | 0.451 |
| push | 786.597 | 737.117 | 791.025 | 1.006 | 1.088 |
| watch | 360.627 | 368.568 | 375.522 | 1.041 | 0.986 |
| fetch-batch | 6717.184 | 6030.175 | 2935.637 | 0.437 | 0.487 |
| workspace-create | 1637.648 | 1772.722 | 1714.302 | 1.021 | 1.008 |
| ephemeral-job | 813.740 | 799.437 | 820.534 | 0.983 | 1.032 |
