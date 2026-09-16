# Retained caller diagnostics

## apfs-callers

All timed fetch iterations are retained. Calibration is excluded; no timed outliers are removed.

| Case | Build | Round | Iteration milliseconds |
|---|---|---:|---|
| fetch-false | baseline-a | 0 | 105.8, 99.8, 113.8, 98.9, 100.7 |
| fetch-false | baseline-b | 0 | 106.8, 100.7, 98.7, 92.8, 97.8 |
| fetch-false | candidate | 0 | 102.9, 96.8, 104.9, 99.8, 106.7 |
| fetch-true | baseline-a | 0 | 201.0, 194.8, 193.7, 194.8, 189.7 |
| fetch-true | baseline-b | 0 | 210.8, 194.9, 198.9, 199.8, 193.6 |
| fetch-true | candidate | 0 | 204.8, 196.9, 209.7, 185.8, 197.8 |
| fetch-false | baseline-a | 1 | 99.8, 97.1, 97.7, 97.3, 97.7 |
| fetch-false | candidate | 1 | 103.9, 97.7, 99.9, 100.7, 100.8 |
| fetch-false | baseline-b | 1 | 105.9, 99.7, 101.7, 100.6, 92.8 |
| fetch-true | baseline-a | 1 | 204.8, 195.8, 196.7, 201.8, 203.0 |
| fetch-true | candidate | 1 | 213.7, 201.8, 192.9, 194.7, 194.7 |
| fetch-true | baseline-b | 1 | 214.6, 181.7, 191.7, 192.7, 197.8 |
| fetch-false | candidate | 2 | 98.7, 101.9, 100.7, 103.7, 101.9 |
| fetch-false | baseline-a | 2 | 105.8, 95.5, 101.8, 103.8, 98.8 |
| fetch-false | baseline-b | 2 | 105.8, 101.7, 100.8, 94.7, 99.7 |
| fetch-true | candidate | 2 | 202.9, 143.1, 195.8, 202.9, 190.9 |
| fetch-true | baseline-a | 2 | 167.4, 158.6, 194.8, 193.8, 146.2 |
| fetch-true | baseline-b | 2 | 163.6, 182.1, 154.6, 162.8, 135.1 |
| fetch-false | candidate | 3 | 102.8, 99.8, 98.8, 99.7, 100.7 |
| fetch-false | baseline-b | 3 | 104.7, 90.7, 101.7, 109.8, 93.8 |
| fetch-false | baseline-a | 3 | 96.9, 91.7, 105.7, 97.9, 95.7 |
| fetch-true | candidate | 3 | 199.1, 196.8, 195.9, 192.8, 201.7 |
| fetch-true | baseline-b | 3 | 207.7, 196.8, 192.9, 194.7, 187.8 |
| fetch-true | baseline-a | 3 | 196.9, 189.8, 195.8, 204.7, 186.8 |
| fetch-false | baseline-b | 4 | 105.8, 94.7, 97.7, 97.8, 101.7 |
| fetch-false | candidate | 4 | 148.3, 100.7, 104.7, 95.7, 104.4 |
| fetch-false | baseline-a | 4 | 98.7, 97.8, 95.8, 96.8, 104.8 |
| fetch-true | baseline-b | 4 | 209.5, 192.8, 186.8, 192.8, 199.8 |
| fetch-true | candidate | 4 | 199.0, 190.7, 190.7, 193.7, 191.7 |
| fetch-true | baseline-a | 4 | 206.8, 203.8, 192.8, 190.9, 193.9 |
| fetch-false | baseline-b | 5 | 103.7, 101.7, 100.7, 99.8, 99.7 |
| fetch-false | baseline-a | 5 | 104.7, 100.7, 102.7, 93.8, 100.7 |
| fetch-false | candidate | 5 | 92.8, 102.4, 98.9, 93.8, 97.4 |
| fetch-true | baseline-b | 5 | 212.9, 197.8, 191.7, 196.8, 192.7 |
| fetch-true | baseline-a | 5 | 202.5, 203.8, 192.4, 190.9, 198.7 |
| fetch-true | candidate | 5 | 206.7, 168.7, 194.7, 188.7, 201.7 |
| fetch-false | baseline-a | 6 | 106.7, 98.7, 92.7, 99.7, 103.9 |
| fetch-false | baseline-b | 6 | 106.7, 101.9, 108.0, 98.7, 97.7 |
| fetch-false | candidate | 6 | 109.7, 98.9, 103.3, 98.7, 92.7 |
| fetch-true | baseline-a | 6 | 202.7, 210.7, 195.7, 203.8, 192.8 |
| fetch-true | baseline-b | 6 | 204.7, 196.9, 188.7, 193.9, 191.6 |
| fetch-true | candidate | 6 | 217.8, 194.8, 185.7, 198.7, 191.8 |
| fetch-false | baseline-a | 7 | 105.7, 100.7, 94.8, 96.9, 98.7 |
| fetch-false | candidate | 7 | 104.8, 97.6, 100.8, 94.7, 101.7 |
| fetch-false | baseline-b | 7 | 102.8, 108.9, 105.7, 97.8, 96.8 |
| fetch-true | baseline-a | 7 | 204.8, 194.8, 191.7, 192.5, 198.8 |
| fetch-true | candidate | 7 | 206.6, 199.8, 199.8, 199.7, 190.9 |
| fetch-true | baseline-b | 7 | 202.7, 196.8, 195.6, 187.7, 192.7 |
| fetch-false | candidate | 8 | 106.0, 100.8, 100.7, 101.7, 100.8 |
| fetch-false | baseline-a | 8 | 108.4, 96.8, 99.8, 98.7, 109.8 |
| fetch-false | baseline-b | 8 | 95.6, 91.8, 96.9, 102.7, 99.7 |
| fetch-true | candidate | 8 | 150.2, 192.0, 140.5, 191.7, 138.6 |
| fetch-true | baseline-a | 8 | 205.8, 202.7, 131.3, 115.6, 127.1 |
| fetch-true | baseline-b | 8 | 203.8, 159.5, 196.8, 193.9, 187.8 |
| fetch-false | candidate | 9 | 100.8, 99.9, 96.7, 97.8, 98.7 |
| fetch-false | baseline-b | 9 | 106.9, 95.0, 99.6, 95.8, 101.9 |
| fetch-false | baseline-a | 9 | 100.8, 94.9, 96.7, 101.7, 96.7 |
| fetch-true | candidate | 9 | 215.2, 193.7, 197.7, 194.7, 196.7 |
| fetch-true | baseline-b | 9 | 207.7, 195.3, 202.8, 188.7, 193.7 |
| fetch-true | baseline-a | 9 | 202.8, 193.7, 191.8, 186.7, 196.7 |
| fetch-false | baseline-b | 10 | 101.7, 103.0, 95.8, 98.8, 96.8 |
| fetch-false | candidate | 10 | 107.8, 95.8, 105.7, 101.8, 100.7 |
| fetch-false | baseline-a | 10 | 101.8, 98.8, 96.4, 97.7, 99.7 |
| fetch-true | baseline-b | 10 | 201.8, 203.7, 192.8, 188.9, 188.8 |
| fetch-true | candidate | 10 | 208.7, 193.0, 190.8, 204.7, 192.7 |
| fetch-true | baseline-a | 10 | 194.8, 198.8, 192.8, 200.8, 189.8 |
| fetch-false | baseline-b | 11 | 101.9, 99.7, 98.7, 99.9, 98.8 |
| fetch-false | baseline-a | 11 | 69.9, 70.5, 52.1, 67.3, 51.2 |
| fetch-false | candidate | 11 | 98.6, 68.2, 54.0, 96.7, 76.2 |
| fetch-true | baseline-b | 11 | 203.1, 196.8, 192.6, 191.5, 195.9 |
| fetch-true | baseline-a | 11 | 199.4, 192.8, 193.0, 197.8, 192.8 |
| fetch-true | candidate | 11 | 202.8, 192.8, 193.6, 189.7, 190.8 |

## btrfs-callers

All timed fetch iterations are retained. Calibration is excluded; no timed outliers are removed.

| Case | Build | Round | Iteration milliseconds |
|---|---|---:|---|
| fetch-false | baseline-a | 0 | 91.0, 80.5, 183.7, 147.7, 81.0 |
| fetch-false | baseline-b | 0 | 97.8, 89.1, 89.5, 89.1, 135.4 |
| fetch-false | candidate | 0 | 92.8, 90.1, 92.4, 92.1, 94.0 |
| fetch-true | baseline-a | 0 | 217.2, 187.2, 203.1, 194.7, 185.1 |
| fetch-true | baseline-b | 0 | 201.0, 198.1, 193.5, 201.6, 202.9 |
| fetch-true | candidate | 0 | 213.6, 206.0, 206.0, 186.5, 204.5 |
| fetch-false | baseline-a | 1 | 98.3, 86.3, 88.2, 83.1, 84.4 |
| fetch-false | candidate | 1 | 99.2, 83.6, 149.6, 82.7, 90.0 |
| fetch-false | baseline-b | 1 | 105.6, 91.2, 83.1, 83.3, 82.7 |
| fetch-true | baseline-a | 1 | 222.4, 185.7, 211.6, 334.1, 185.9 |
| fetch-true | candidate | 1 | 207.9, 197.9, 193.6, 210.9, 199.5 |
| fetch-true | baseline-b | 1 | 269.3, 500.2, 199.3, 193.5, 191.2 |
| fetch-false | candidate | 2 | 104.0, 85.6, 160.1, 221.1, 88.6 |
| fetch-false | baseline-a | 2 | 95.2, 91.4, 144.8, 87.6, 93.8 |
| fetch-false | baseline-b | 2 | 94.7, 89.9, 90.1, 89.4, 90.6 |
| fetch-true | candidate | 2 | 192.6, 203.9, 339.6, 192.4, 198.2 |
| fetch-true | baseline-a | 2 | 221.5, 198.6, 188.8, 192.8, 200.1 |
| fetch-true | baseline-b | 2 | 951.3, 366.5, 204.3, 256.0, 189.8 |
| fetch-false | candidate | 3 | 89.8, 81.1, 79.9, 83.8, 82.7 |
| fetch-false | baseline-b | 3 | 95.4, 84.0, 81.9, 85.9, 241.0 |
| fetch-false | baseline-a | 3 | 98.6, 90.5, 87.6, 154.3, 89.6 |
| fetch-true | candidate | 3 | 203.0, 191.1, 203.9, 187.9, 189.0 |
| fetch-true | baseline-b | 3 | 359.0, 188.2, 214.0, 195.1, 204.7 |
| fetch-true | baseline-a | 3 | 194.6, 185.8, 189.6, 198.1, 202.3 |
| fetch-false | baseline-b | 4 | 91.9, 79.6, 85.9, 85.3, 83.5 |
| fetch-false | candidate | 4 | 94.4, 89.8, 88.3, 88.8, 90.2 |
| fetch-false | baseline-a | 4 | 133.8, 90.3, 87.2, 87.0, 89.5 |
| fetch-true | baseline-b | 4 | 216.6, 191.9, 205.2, 210.8, 199.7 |
| fetch-true | candidate | 4 | 216.8, 213.0, 207.8, 192.0, 209.4 |
| fetch-true | baseline-a | 4 | 216.1, 184.2, 206.8, 184.8, 203.1 |
| fetch-false | baseline-b | 5 | 98.2, 89.6, 94.4, 89.2, 89.6 |
| fetch-false | baseline-a | 5 | 91.2, 83.0, 80.5, 85.2, 85.4 |
| fetch-false | candidate | 5 | 92.7, 81.0, 83.8, 90.5, 79.9 |
| fetch-true | baseline-b | 5 | 207.2, 201.2, 206.5, 209.8, 205.3 |
| fetch-true | baseline-a | 5 | 369.1, 189.5, 200.5, 195.9, 188.9 |
| fetch-true | candidate | 5 | 201.2, 195.0, 194.5, 198.8, 210.4 |
| fetch-false | baseline-a | 6 | 852.0, 88.6, 97.4, 94.2, 87.6 |
| fetch-false | baseline-b | 6 | 99.3, 83.5, 83.2, 88.7, 89.4 |
| fetch-false | candidate | 6 | 99.7, 106.4, 87.8, 83.5, 84.2 |
| fetch-true | baseline-a | 6 | 239.8, 199.0, 211.9, 207.2, 187.4 |
| fetch-true | baseline-b | 6 | 349.5, 201.3, 202.3, 205.0, 187.9 |
| fetch-true | candidate | 6 | 199.7, 190.9, 192.7, 203.3, 200.0 |
| fetch-false | baseline-a | 7 | 99.8, 91.5, 89.4, 90.7, 90.8 |
| fetch-false | candidate | 7 | 100.4, 155.6, 85.8, 90.2, 91.5 |
| fetch-false | baseline-b | 7 | 106.7, 83.9, 83.9, 83.7, 86.5 |
| fetch-true | baseline-a | 7 | 204.2, 215.4, 331.6, 206.2, 271.8 |
| fetch-true | candidate | 7 | 200.0, 184.0, 202.5, 194.6, 192.0 |
| fetch-true | baseline-b | 7 | 220.8, 186.6, 200.7, 203.0, 193.1 |
| fetch-false | candidate | 8 | 96.9, 89.5, 85.9, 87.1, 89.8 |
| fetch-false | baseline-a | 8 | 117.3, 82.5, 82.8, 81.2, 86.2 |
| fetch-false | baseline-b | 8 | 104.4, 91.2, 91.5, 91.5, 92.3 |
| fetch-true | candidate | 8 | 196.9, 194.5, 201.1, 203.1, 182.2 |
| fetch-true | baseline-a | 8 | 207.3, 189.6, 188.4, 209.3, 203.6 |
| fetch-true | baseline-b | 8 | 207.6, 265.4, 242.5, 197.6, 190.4 |
| fetch-false | candidate | 9 | 98.6, 155.3, 84.9, 83.9, 82.7 |
| fetch-false | baseline-b | 9 | 104.4, 87.0, 91.4, 92.4, 91.5 |
| fetch-false | baseline-a | 9 | 99.2, 90.8, 89.0, 145.9, 126.7 |
| fetch-true | candidate | 9 | 221.0, 187.6, 200.9, 191.2, 197.5 |
| fetch-true | baseline-b | 9 | 208.9, 195.6, 197.9, 292.0, 216.7 |
| fetch-true | baseline-a | 9 | 204.7, 203.9, 189.5, 204.0, 192.8 |
| fetch-false | baseline-b | 10 | 97.8, 79.9, 88.2, 84.1, 83.6 |
| fetch-false | candidate | 10 | 94.5, 83.7, 155.1, 83.4, 85.5 |
| fetch-false | baseline-a | 10 | 871.6, 80.4, 89.3, 87.8, 90.3 |
| fetch-true | baseline-b | 10 | 201.9, 198.7, 187.7, 190.3, 189.0 |
| fetch-true | candidate | 10 | 497.2, 206.6, 211.4, 195.4, 187.6 |
| fetch-true | baseline-a | 10 | 214.9, 205.0, 204.4, 398.3, 201.2 |
| fetch-false | baseline-b | 11 | 93.4, 89.4, 94.5, 91.8, 94.1 |
| fetch-false | baseline-a | 11 | 92.9, 143.3, 89.7, 89.0, 86.4 |
| fetch-false | candidate | 11 | 98.3, 135.8, 82.0, 80.9, 81.5 |
| fetch-true | baseline-b | 11 | 206.5, 197.3, 187.5, 192.4, 259.3 |
| fetch-true | baseline-a | 11 | 216.2, 204.8, 192.9, 892.9, 482.8 |
| fetch-true | candidate | 11 | 212.9, 202.2, 189.2, 205.3, 204.1 |

