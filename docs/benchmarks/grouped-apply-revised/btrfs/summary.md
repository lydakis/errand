# Grouped apply comparison

Individual complete-operation timings; A/B execute the same frozen baseline binary.

| Case | Baseline A ms | Baseline B ms | Candidate ms | Median paired C/A | Median paired C/B |
|---|---:|---:|---:|---:|---:|
| apply-1 | 124.685 | 126.106 | 129.530 | 1.033 | 1.074 |
| apply-8 | 383.135 | 388.124 | 287.292 | 0.742 | 0.743 |
| apply-32 | 1284.535 | 1305.646 | 832.534 | 0.650 | 0.643 |
| apply-128 | 6106.504 | 5815.740 | 2908.301 | 0.479 | 0.497 |
| apply-512 | 24152.654 | 24488.640 | 12137.566 | 0.504 | 0.496 |
| push | 749.431 | 782.604 | 735.942 | 1.031 | 1.022 |
| watch | 370.602 | 406.995 | 378.213 | 1.021 | 0.893 |
| fetch-batch | 6163.521 | 6064.061 | 3644.872 | 0.592 | 0.555 |
| workspace-create | 1596.541 | 1620.122 | 1571.677 | 1.006 | 0.970 |
| ephemeral-job | 820.119 | 820.680 | 799.886 | 0.975 | 0.975 |
