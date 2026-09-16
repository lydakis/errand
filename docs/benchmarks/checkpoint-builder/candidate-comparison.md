# Candidate before allocation fix

## Native comparison results

The main campaigns completed successfully with 720 samples per host: 160
four-way rounds plus 40 ordinary-builder pairs. All roots and counters match.
Tables show median milliseconds. Ratios are medians of within-round ratios,
not ratios of the displayed medians. Lower is faster. Position splits, ranges
and phase medians are retained in [summary.json](summary.json).

| Host | Files / bytes / selection | Case | Frozen checkpoint | Shared update | Update / frozen | Wins | Shared current | Current / update |
|---|---|---|---:|---:|---:|---:|---:|---:|
| apfs | 1000-32-ignore | miss | 57.56 | 57.56 | 0.817 | 6/8 | 65.78 | 1.270 |
| apfs | 1000-32-ignore | unchanged | 44.62 | 38.22 | 0.969 | 4/8 | 43.88 | 1.081 |
| apfs | 1000-32-ignore | edit | 44.23 | 37.45 | 0.850 | 5/8 | 35.82 | 0.893 |
| apfs | 1000-32-ignore | batch | 42.40 | 56.55 | 1.323 | 2/8 | 62.00 | 0.948 |
| apfs | 10000-4096-ignore | miss | 213.75 | 192.34 | 0.913 | 5/8 | 193.69 | 0.975 |
| apfs | 10000-4096-ignore | unchanged | 97.35 | 102.34 | 0.952 | 4/8 | 93.84 | 0.984 |
| apfs | 10000-4096-ignore | edit | 89.77 | 91.76 | 0.994 | 4/8 | 93.20 | 0.985 |
| apfs | 10000-4096-ignore | batch | 107.67 | 91.27 | 0.909 | 6/8 | 99.50 | 1.013 |
| apfs | 1000-65536-ignore | miss | 72.01 | 72.91 | 1.027 | 4/8 | 76.14 | 0.949 |
| apfs | 1000-65536-ignore | unchanged | 43.23 | 42.88 | 0.921 | 6/8 | 46.28 | 0.972 |
| apfs | 1000-65536-ignore | edit | 35.73 | 38.13 | 0.931 | 4/8 | 43.31 | 1.040 |
| apfs | 1000-65536-ignore | batch | 68.84 | 65.39 | 0.981 | 5/8 | 65.74 | 1.000 |
| apfs | 50000-128-ignore | miss | 858.03 | 794.25 | 0.939 | 8/8 | 787.53 | 0.980 |
| apfs | 50000-128-ignore | unchanged | 269.40 | 251.36 | 0.974 | 7/8 | 253.40 | 0.999 |
| apfs | 50000-128-ignore | edit | 289.83 | 285.02 | 0.975 | 5/8 | 285.22 | 1.025 |
| apfs | 50000-128-ignore | batch | 291.01 | 299.45 | 0.978 | 5/8 | 296.79 | 1.016 |
| apfs | 10000-4096-git | miss | 376.05 | 381.85 | 1.002 | 4/8 | 366.09 | 0.981 |
| apfs | 10000-4096-git | unchanged | 274.75 | 268.26 | 0.992 | 5/8 | 259.73 | 0.955 |
| apfs | 10000-4096-git | edit | 251.93 | 273.49 | 0.991 | 5/8 | 284.30 | 1.128 |
| apfs | 10000-4096-git | batch | 318.73 | 323.57 | 1.039 | 3/8 | 274.92 | 0.897 |
| btrfs | 1000-32-ignore | miss | 26.49 | 24.10 | 0.907 | 8/8 | 23.31 | 0.982 |
| btrfs | 1000-32-ignore | unchanged | 13.82 | 13.78 | 0.994 | 5/8 | 14.16 | 1.038 |
| btrfs | 1000-32-ignore | edit | 16.41 | 16.12 | 0.989 | 6/8 | 16.28 | 1.009 |
| btrfs | 1000-32-ignore | batch | 30.92 | 28.56 | 0.926 | 7/8 | 27.55 | 0.970 |
| btrfs | 10000-4096-ignore | miss | 482.54 | 446.82 | 0.955 | 6/8 | 432.50 | 0.968 |
| btrfs | 10000-4096-ignore | unchanged | 115.38 | 114.96 | 1.026 | 3/8 | 110.32 | 0.953 |
| btrfs | 10000-4096-ignore | edit | 135.13 | 127.33 | 0.959 | 5/8 | 129.30 | 1.024 |
| btrfs | 10000-4096-ignore | batch | 160.70 | 163.89 | 1.024 | 3/8 | 153.55 | 0.933 |
| btrfs | 1000-65536-ignore | miss | 287.82 | 260.50 | 0.934 | 7/8 | 279.08 | 1.034 |
| btrfs | 1000-65536-ignore | unchanged | 14.14 | 13.75 | 0.961 | 7/8 | 13.92 | 1.013 |
| btrfs | 1000-65536-ignore | edit | 16.66 | 16.66 | 1.007 | 4/8 | 16.59 | 1.003 |
| btrfs | 1000-65536-ignore | batch | 253.34 | 248.30 | 0.948 | 5/8 | 241.54 | 0.952 |
| btrfs | 50000-128-ignore | miss | 1232.98 | 1100.61 | 0.886 | 8/8 | 1065.30 | 0.995 |
| btrfs | 50000-128-ignore | unchanged | 535.03 | 534.07 | 1.023 | 3/8 | 538.36 | 1.008 |
| btrfs | 50000-128-ignore | edit | 601.34 | 600.31 | 0.996 | 5/8 | 606.03 | 1.029 |
| btrfs | 50000-128-ignore | batch | 632.27 | 600.79 | 0.972 | 7/8 | 619.94 | 1.040 |
| btrfs | 10000-4096-git | miss | 717.18 | 728.34 | 0.933 | 5/8 | 661.45 | 0.931 |
| btrfs | 10000-4096-git | unchanged | 303.09 | 297.25 | 0.992 | 4/8 | 303.49 | 1.021 |
| btrfs | 10000-4096-git | edit | 328.62 | 318.89 | 0.972 | 5/8 | 319.70 | 1.001 |
| btrfs | 10000-4096-git | batch | 410.91 | 392.19 | 0.959 | 6/8 | 398.07 | 1.019 |

The clearest improvement is 50K initial population: shared-update wins all
eight rounds on each host. Warm changes are generally small. The ordinary
stat inventory and fresh selection checks remain substantial costs. For example,
the 50K APFS unchanged shared pass spends about 69 ms in inventory/build, versus
about 10 ms in index preparation. On Cabal those phase medians are about 149 ms
and 59 ms. Phase medians do not sum to the total median.

Both shared modes execute the same miss path. Differences between their miss
measurements reflect run/cache variability, not different initial-population
algorithms. Direct current-state construction has no consistent warm advantage
across these fixtures. This experiment keeps prior-state construction plus edits
as its default; the direct alternative remains explicitly selectable for research.

### Ordinary-builder guard

| Host | Fixture | Frozen cold ms | Candidate cold ms | Paired ratio | Candidate wins |
|---|---|---:|---:|---:|---:|
| apfs | 1000-32-ignore | 47.81 | 47.96 | 0.998 | 4/8 |
| apfs | 10000-4096-ignore | 180.47 | 192.68 | 1.004 | 4/8 |
| apfs | 1000-65536-ignore | 92.71 | 91.13 | 0.995 | 4/8 |
| apfs | 50000-128-ignore | 730.63 | 739.61 | 1.008 | 4/8 |
| apfs | 10000-4096-git | 397.30 | 391.06 | 1.047 | 3/8 |
| btrfs | 1000-32-ignore | 20.62 | 20.46 | 0.992 | 6/8 |
| btrfs | 10000-4096-ignore | 452.11 | 441.59 | 0.905 | 5/8 |
| btrfs | 1000-65536-ignore | 246.02 | 281.98 | 1.163 | 1/8 |
| btrfs | 50000-128-ignore | 937.39 | 909.40 | 0.984 | 7/8 |
| btrfs | 10000-4096-git | 749.32 | 769.28 | 1.044 | 1/8 |

The Cabal 64-KiB and Git guard results and the APFS small batch result triggered
a separate 24-round A/A/B recheck. They are retained rather than averaged away.
That recheck uses two identical frozen-baseline labels and the candidate,
balances all six orders, and verifies source/harness/script identity and roots.
The results are reported separately below.

