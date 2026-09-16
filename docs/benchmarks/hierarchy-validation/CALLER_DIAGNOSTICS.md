# Retained caller diagnostics

## apfs-final

All timed fetch iterations are retained. Calibration is excluded; no timed outliers are removed.

| Case | Build | Round | Iteration milliseconds |
|---|---|---:|---|
| fetch-false | baseline | 0 | 101.7, 96.6, 95.8, 99.8, 99.7 |
| fetch-false | candidate | 0 | 105.9, 91.8, 101.9, 97.9, 105.7 |
| fetch-true | baseline | 0 | 204.0, 199.8, 197.3, 195.8, 200.8 |
| fetch-true | candidate | 0 | 208.8, 201.9, 199.7, 194.8, 201.7 |
| fetch-false | candidate | 1 | 94.7, 95.8, 94.7, 98.8, 103.8 |
| fetch-false | baseline | 1 | 102.0, 94.7, 97.9, 95.9, 100.8 |
| fetch-true | candidate | 1 | 205.8, 193.8, 192.8, 198.8, 193.7 |
| fetch-true | baseline | 1 | 211.8, 201.3, 191.3, 193.9, 200.8 |
| fetch-false | baseline | 2 | 107.8, 92.8, 101.7, 96.8, 97.9 |
| fetch-false | candidate | 2 | 100.5, 93.7, 99.7, 102.8, 97.6 |
| fetch-true | baseline | 2 | 206.8, 192.7, 202.8, 195.9, 194.8 |
| fetch-true | candidate | 2 | 207.7, 187.9, 199.8, 188.7, 196.8 |
| fetch-false | candidate | 3 | 100.9, 99.7, 97.7, 98.7, 97.7 |
| fetch-false | baseline | 3 | 104.8, 99.8, 100.8, 93.9, 99.8 |
| fetch-true | candidate | 3 | 203.3, 197.8, 195.8, 191.8, 199.7 |
| fetch-true | baseline | 3 | 202.8, 197.9, 195.7, 191.8, 193.6 |
| fetch-false | baseline | 4 | 100.8, 101.7, 95.7, 94.8, 101.7 |
| fetch-false | candidate | 4 | 98.7, 95.9, 99.7, 101.8, 100.7 |
| fetch-true | baseline | 4 | 210.5, 197.9, 191.8, 190.7, 188.8 |
| fetch-true | candidate | 4 | 203.9, 191.8, 197.9, 192.8, 192.0 |
| fetch-false | candidate | 5 | 100.7, 94.7, 99.7, 94.8, 96.7 |
| fetch-false | baseline | 5 | 99.7, 97.7, 98.1, 104.8, 98.7 |
| fetch-true | candidate | 5 | 210.8, 195.7, 197.8, 190.8, 191.7 |
| fetch-true | baseline | 5 | 205.8, 193.8, 198.8, 186.6, 184.5 |

| Burst metric | Baseline median | Candidate median | Median paired ratio |
|---|---:|---:|---:|
| ns/op | 3454992704.000 | 3525284358.500 | 1.038 |
| settle-ms/op | 563.400 | 576.050 | 1.049 |
| cpu-ms/op | 2413.500 | 2469.000 | 1.046 |
| client-ms/op | 3231.500 | 3301.500 | 1.044 |
| stage-ms/op | 63.930 | 69.985 | 1.101 |
| apply-ms/op | 161.100 | 173.950 | 1.095 |
| resamples/op | 10.900 | 11.000 | 1.029 |

## btrfs-final

All timed fetch iterations are retained. Calibration is excluded; no timed outliers are removed.

| Case | Build | Round | Iteration milliseconds |
|---|---|---:|---|
| fetch-false | baseline | 0 | 95.4, 82.7, 81.8, 82.4, 83.1 |
| fetch-false | candidate | 0 | 160.6, 136.2, 81.3, 81.4, 91.0 |
| fetch-true | baseline | 0 | 203.1, 186.3, 199.4, 184.1, 205.7 |
| fetch-true | candidate | 0 | 199.2, 299.0, 195.9, 189.0, 185.2 |
| fetch-false | candidate | 1 | 172.2, 83.6, 710.9, 85.7, 91.8 |
| fetch-false | baseline | 1 | 99.9, 88.2, 87.4, 88.2, 87.8 |
| fetch-true | candidate | 1 | 313.4, 217.4, 333.4, 196.1, 202.9 |
| fetch-true | baseline | 1 | 219.2, 202.2, 196.9, 206.4, 209.8 |
| fetch-false | baseline | 2 | 174.5, 82.8, 82.0, 82.3, 83.4 |
| fetch-false | candidate | 2 | 91.0, 90.0, 82.0, 80.7, 112.4 |
| fetch-true | baseline | 2 | 200.5, 207.0, 231.4, 201.5, 192.1 |
| fetch-true | candidate | 2 | 208.0, 298.9, 194.2, 184.8, 204.1 |
| fetch-false | candidate | 3 | 93.5, 92.3, 92.4, 92.2, 95.8 |
| fetch-false | baseline | 3 | 102.4, 245.3, 90.1, 136.6, 92.1 |
| fetch-true | candidate | 3 | 195.3, 184.9, 197.2, 199.4, 201.7 |
| fetch-true | baseline | 3 | 341.6, 307.5, 197.9, 196.4, 194.2 |
| fetch-false | baseline | 4 | 151.4, 89.1, 89.0, 79.4, 86.2 |
| fetch-false | candidate | 4 | 97.4, 82.0, 81.6, 156.4, 85.9 |
| fetch-true | baseline | 4 | 219.4, 199.8, 188.3, 235.6, 222.5 |
| fetch-true | candidate | 4 | 203.3, 209.5, 200.8, 925.7, 194.4 |
| fetch-false | candidate | 5 | 95.0, 81.6, 164.1, 82.6, 160.1 |
| fetch-false | baseline | 5 | 99.2, 86.9, 89.1, 94.1, 97.7 |
| fetch-true | candidate | 5 | 222.5, 194.8, 204.9, 189.0, 189.0 |
| fetch-true | baseline | 5 | 205.0, 205.3, 292.9, 195.6, 193.3 |

| Burst metric | Baseline median | Candidate median | Median paired ratio |
|---|---:|---:|---:|
| ns/op | 3033237389.000 | 3036656311.000 | 1.009 |
| settle-ms/op | 798.400 | 796.200 | 1.033 |
| cpu-ms/op | 2763.500 | 2783.500 | 1.006 |
| client-ms/op | 2712.000 | 2719.500 | 1.005 |
| stage-ms/op | 146.250 | 148.550 | 1.020 |
| apply-ms/op | 170.900 | 177.100 | 1.091 |
| resamples/op | 8.000 | 8.000 | 1.012 |

