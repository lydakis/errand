# Hierarchy follow-up comparisons

Lower is better. Ratios use matched rounds; ranges are observed extrema, not confidence intervals. All six build orders are equally represented. Baseline-a and baseline-b execute the same binary.

## apfs-callers

Reference: pinned published baseline. Baseline-a and baseline-b execute the same binary; candidate is the final source recorded in this report.

| Case / metric | Comparison | Paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| update-1000-dispersed / ns/op | baseline-b/baseline-a | 1.004 | 0.956–1.052 | 5/12 |
| update-1000-dispersed / ns/op | candidate/baseline-a | 0.529 | 0.511–0.538 | 12/12 |
| update-1000-dispersed / ns/op | candidate/baseline-b | 0.528 | 0.498–0.534 | 12/12 |
| update-1000-dispersed / B/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 4/12 |
| update-1000-dispersed / B/op | candidate/baseline-a | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-dispersed / B/op | candidate/baseline-b | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-dispersed / allocs/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 0/12 |
| update-1000-dispersed / allocs/op | candidate/baseline-a | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-dispersed / allocs/op | candidate/baseline-b | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-mixed / ns/op | baseline-b/baseline-a | 0.998 | 0.981–1.025 | 7/12 |
| update-1000-mixed / ns/op | candidate/baseline-a | 0.940 | 0.916–0.952 | 12/12 |
| update-1000-mixed / ns/op | candidate/baseline-b | 0.936 | 0.922–0.946 | 12/12 |
| update-1000-mixed / B/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 5/12 |
| update-1000-mixed / B/op | candidate/baseline-a | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-mixed / B/op | candidate/baseline-b | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-mixed / allocs/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 0/12 |
| update-1000-mixed / allocs/op | candidate/baseline-a | 0.767 | 0.767–0.767 | 12/12 |
| update-1000-mixed / allocs/op | candidate/baseline-b | 0.767 | 0.767–0.767 | 12/12 |
| watch / ns/op | baseline-b/baseline-a | 0.998 | 0.981–1.025 | 6/12 |
| watch / ns/op | candidate/baseline-a | 1.000 | 0.960–1.032 | 6/12 |
| watch / ns/op | candidate/baseline-b | 0.999 | 0.964–1.023 | 6/12 |
| watch-burst / ns/op | baseline-b/baseline-a | 0.989 | 0.938–1.085 | 10/12 |
| watch-burst / ns/op | candidate/baseline-a | 0.947 | 0.920–1.129 | 8/12 |
| watch-burst / ns/op | candidate/baseline-b | 0.978 | 0.915–1.130 | 8/12 |
| watch-burst / settle-ms/op | baseline-b/baseline-a | 1.002 | 0.896–1.109 | 5/12 |
| watch-burst / settle-ms/op | candidate/baseline-a | 1.007 | 0.886–1.092 | 5/12 |
| watch-burst / settle-ms/op | candidate/baseline-b | 0.983 | 0.954–1.135 | 7/12 |
| watch-burst / cpu-ms/op | baseline-b/baseline-a | 0.978 | 0.941–1.069 | 8/12 |
| watch-burst / cpu-ms/op | candidate/baseline-a | 0.972 | 0.928–1.102 | 7/12 |
| watch-burst / cpu-ms/op | candidate/baseline-b | 0.984 | 0.920–1.103 | 9/12 |
| watch-burst / client-ms/op | baseline-b/baseline-a | 0.978 | 0.934–1.065 | 8/12 |
| watch-burst / client-ms/op | candidate/baseline-a | 0.976 | 0.922–1.101 | 7/12 |
| watch-burst / client-ms/op | candidate/baseline-b | 0.987 | 0.920–1.102 | 9/12 |
| watch-burst / stage-ms/op | baseline-b/baseline-a | 1.084 | 0.570–1.415 | 5/12 |
| watch-burst / stage-ms/op | candidate/baseline-a | 0.996 | 0.500–1.475 | 6/12 |
| watch-burst / stage-ms/op | candidate/baseline-b | 0.860 | 0.677–1.499 | 10/12 |
| watch-burst / apply-ms/op | baseline-b/baseline-a | 1.065 | 0.434–1.388 | 4/12 |
| watch-burst / apply-ms/op | candidate/baseline-a | 0.970 | 0.495–1.508 | 7/12 |
| watch-burst / apply-ms/op | candidate/baseline-b | 0.868 | 0.708–1.492 | 7/12 |
| watch-burst / resamples/op | baseline-b/baseline-a | 0.982 | 0.915–1.115 | 7/12 |
| watch-burst / resamples/op | candidate/baseline-a | 0.982 | 0.912–1.094 | 6/12 |
| watch-burst / resamples/op | candidate/baseline-b | 0.991 | 0.927–1.094 | 6/12 |
| fetch-false / ns/op | baseline-b/baseline-a | 1.012 | 0.948–1.604 | 3/12 |
| fetch-false / ns/op | candidate/baseline-a | 1.006 | 0.966–1.266 | 3/12 |
| fetch-false / ns/op | candidate/baseline-b | 1.003 | 0.789–1.113 | 5/12 |
| fetch-true / ns/op | baseline-b/baseline-a | 1.001 | 0.927–1.203 | 6/12 |
| fetch-true / ns/op | candidate/baseline-a | 1.013 | 0.972–1.087 | 5/12 |
| fetch-true / ns/op | candidate/baseline-b | 1.008 | 0.863–1.172 | 5/12 |

## btrfs-callers

Reference: pinned published baseline. Baseline-a and baseline-b execute the same binary; candidate is the final source recorded in this report.

| Case / metric | Comparison | Paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| update-1000-dispersed / ns/op | baseline-b/baseline-a | 1.118 | 0.905–1.395 | 3/12 |
| update-1000-dispersed / ns/op | candidate/baseline-a | 0.616 | 0.463–0.863 | 12/12 |
| update-1000-dispersed / ns/op | candidate/baseline-b | 0.529 | 0.415–0.778 | 12/12 |
| update-1000-dispersed / B/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 4/12 |
| update-1000-dispersed / B/op | candidate/baseline-a | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-dispersed / B/op | candidate/baseline-b | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-dispersed / allocs/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 0/12 |
| update-1000-dispersed / allocs/op | candidate/baseline-a | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-dispersed / allocs/op | candidate/baseline-b | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-mixed / ns/op | baseline-b/baseline-a | 0.969 | 0.744–1.386 | 8/12 |
| update-1000-mixed / ns/op | candidate/baseline-a | 0.996 | 0.713–1.168 | 6/12 |
| update-1000-mixed / ns/op | candidate/baseline-b | 1.017 | 0.705–1.187 | 5/12 |
| update-1000-mixed / B/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 6/12 |
| update-1000-mixed / B/op | candidate/baseline-a | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-mixed / B/op | candidate/baseline-b | 0.988 | 0.988–0.988 | 12/12 |
| update-1000-mixed / allocs/op | baseline-b/baseline-a | 1.000 | 1.000–1.000 | 0/12 |
| update-1000-mixed / allocs/op | candidate/baseline-a | 0.767 | 0.767–0.767 | 12/12 |
| update-1000-mixed / allocs/op | candidate/baseline-b | 0.767 | 0.767–0.767 | 12/12 |
| watch / ns/op | baseline-b/baseline-a | 0.996 | 0.644–1.353 | 7/12 |
| watch / ns/op | candidate/baseline-a | 1.040 | 0.636–1.318 | 4/12 |
| watch / ns/op | candidate/baseline-b | 1.005 | 0.855–1.342 | 6/12 |
| watch-burst / ns/op | baseline-b/baseline-a | 0.995 | 0.922–1.015 | 9/12 |
| watch-burst / ns/op | candidate/baseline-a | 1.004 | 0.939–1.044 | 5/12 |
| watch-burst / ns/op | candidate/baseline-b | 1.007 | 0.985–1.040 | 4/12 |
| watch-burst / settle-ms/op | baseline-b/baseline-a | 0.975 | 0.749–1.046 | 9/12 |
| watch-burst / settle-ms/op | candidate/baseline-a | 0.999 | 0.792–1.166 | 6/12 |
| watch-burst / settle-ms/op | candidate/baseline-b | 1.025 | 0.951–1.152 | 5/12 |
| watch-burst / cpu-ms/op | baseline-b/baseline-a | 1.003 | 0.987–1.022 | 3/12 |
| watch-burst / cpu-ms/op | candidate/baseline-a | 1.001 | 0.975–1.013 | 5/12 |
| watch-burst / cpu-ms/op | candidate/baseline-b | 0.994 | 0.976–1.015 | 9/12 |
| watch-burst / client-ms/op | baseline-b/baseline-a | 1.002 | 0.962–1.017 | 4/12 |
| watch-burst / client-ms/op | candidate/baseline-a | 0.999 | 0.969–1.015 | 6/12 |
| watch-burst / client-ms/op | candidate/baseline-b | 0.998 | 0.980–1.026 | 7/12 |
| watch-burst / stage-ms/op | baseline-b/baseline-a | 0.963 | 0.764–1.033 | 9/12 |
| watch-burst / stage-ms/op | candidate/baseline-a | 1.047 | 0.748–1.100 | 5/12 |
| watch-burst / stage-ms/op | candidate/baseline-b | 1.048 | 0.954–1.156 | 3/12 |
| watch-burst / apply-ms/op | baseline-b/baseline-a | 0.924 | 0.566–1.021 | 11/12 |
| watch-burst / apply-ms/op | candidate/baseline-a | 1.030 | 0.544–1.600 | 5/12 |
| watch-burst / apply-ms/op | candidate/baseline-b | 1.152 | 0.952–1.652 | 3/12 |
| watch-burst / resamples/op | baseline-b/baseline-a | 1.000 | 1.000–1.026 | 0/12 |
| watch-burst / resamples/op | candidate/baseline-a | 1.000 | 0.925–1.026 | 2/12 |
| watch-burst / resamples/op | candidate/baseline-b | 1.000 | 0.925–1.000 | 2/12 |
| fetch-false / ns/op | baseline-b/baseline-a | 0.905 | 0.356–1.130 | 8/12 |
| fetch-false / ns/op | candidate/baseline-a | 0.940 | 0.378–1.286 | 8/12 |
| fetch-false / ns/op | candidate/baseline-b | 1.049 | 0.710–1.450 | 4/12 |
| fetch-true / ns/op | baseline-b/baseline-a | 1.063 | 0.524–1.964 | 4/12 |
| fetch-true / ns/op | candidate/baseline-a | 0.991 | 0.510–1.125 | 6/12 |
| fetch-true / ns/op | candidate/baseline-b | 0.934 | 0.573–1.342 | 9/12 |

