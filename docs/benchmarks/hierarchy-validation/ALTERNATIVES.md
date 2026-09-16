# Hierarchy follow-up comparisons

Lower is better. Ratios use matched rounds; ranges are observed extrema, not confidence intervals. All six build orders are equally represented. Baseline-a and baseline-b execute the same binary.

## apfs-alternatives

Reference: original hierarchy-preserving candidate. Entry-validation includes preallocation in this campaign.

| Case / metric | Comparison | Paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| update-1000-repeated / ns/op | preallocated/candidate | 0.940 | 0.915–1.004 | 11/12 |
| update-1000-repeated / ns/op | entry-validation/candidate | 0.862 | 0.832–0.902 | 12/12 |
| update-1000-repeated / ns/op | entry-validation/preallocated | 0.911 | 0.894–0.964 | 12/12 |
| update-1000-repeated / B/op | preallocated/candidate | 0.886 | 0.886–0.886 | 12/12 |
| update-1000-repeated / B/op | entry-validation/candidate | 0.884 | 0.884–0.884 | 12/12 |
| update-1000-repeated / B/op | entry-validation/preallocated | 0.998 | 0.998–0.998 | 12/12 |
| update-1000-repeated / allocs/op | preallocated/candidate | 0.708 | 0.708–0.708 | 12/12 |
| update-1000-repeated / allocs/op | entry-validation/candidate | 0.625 | 0.625–0.625 | 12/12 |
| update-1000-repeated / allocs/op | entry-validation/preallocated | 0.882 | 0.882–0.882 | 12/12 |
| update-1000-dispersed / ns/op | preallocated/candidate | 0.965 | 0.922–1.018 | 10/12 |
| update-1000-dispersed / ns/op | entry-validation/candidate | 0.843 | 0.821–0.907 | 12/12 |
| update-1000-dispersed / ns/op | entry-validation/preallocated | 0.884 | 0.846–0.914 | 12/12 |
| update-1000-dispersed / B/op | preallocated/candidate | 0.887 | 0.887–0.887 | 12/12 |
| update-1000-dispersed / B/op | entry-validation/candidate | 0.876 | 0.876–0.876 | 12/12 |
| update-1000-dispersed / B/op | entry-validation/preallocated | 0.987 | 0.987–0.987 | 12/12 |
| update-1000-dispersed / allocs/op | preallocated/candidate | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-dispersed / allocs/op | entry-validation/candidate | 0.517 | 0.517–0.517 | 12/12 |
| update-1000-dispersed / allocs/op | entry-validation/preallocated | 0.682 | 0.682–0.682 | 12/12 |
| update-1000-mixed / ns/op | preallocated/candidate | 0.987 | 0.976–1.008 | 9/12 |
| update-1000-mixed / ns/op | entry-validation/candidate | 0.918 | 0.887–0.961 | 12/12 |
| update-1000-mixed / ns/op | entry-validation/preallocated | 0.930 | 0.898–0.969 | 12/12 |
| update-1000-mixed / B/op | preallocated/candidate | 0.887 | 0.887–0.887 | 12/12 |
| update-1000-mixed / B/op | entry-validation/candidate | 0.876 | 0.876–0.876 | 12/12 |
| update-1000-mixed / B/op | entry-validation/preallocated | 0.987 | 0.987–0.987 | 12/12 |
| update-1000-mixed / allocs/op | preallocated/candidate | 0.767 | 0.767–0.767 | 12/12 |
| update-1000-mixed / allocs/op | entry-validation/candidate | 0.533 | 0.533–0.533 | 12/12 |
| update-1000-mixed / allocs/op | entry-validation/preallocated | 0.696 | 0.696–0.696 | 12/12 |
| update-10000-repeated / ns/op | preallocated/candidate | 0.963 | 0.952–0.975 | 12/12 |
| update-10000-repeated / ns/op | entry-validation/candidate | 0.907 | 0.899–0.917 | 12/12 |
| update-10000-repeated / ns/op | entry-validation/preallocated | 0.940 | 0.934–0.947 | 12/12 |
| update-10000-repeated / B/op | preallocated/candidate | 0.802 | 0.802–0.802 | 12/12 |
| update-10000-repeated / B/op | entry-validation/candidate | 0.799 | 0.799–0.799 | 12/12 |
| update-10000-repeated / B/op | entry-validation/preallocated | 0.996 | 0.996–0.996 | 12/12 |
| update-10000-repeated / allocs/op | preallocated/candidate | 0.946 | 0.946–0.946 | 12/12 |
| update-10000-repeated / allocs/op | entry-validation/candidate | 0.930 | 0.930–0.930 | 12/12 |
| update-10000-repeated / allocs/op | entry-validation/preallocated | 0.984 | 0.984–0.984 | 12/12 |
| update-10000-dispersed / ns/op | preallocated/candidate | 0.984 | 0.980–1.008 | 11/12 |
| update-10000-dispersed / ns/op | entry-validation/candidate | 0.888 | 0.883–0.898 | 12/12 |
| update-10000-dispersed / ns/op | entry-validation/preallocated | 0.900 | 0.882–0.912 | 12/12 |
| update-10000-dispersed / B/op | preallocated/candidate | 0.935 | 0.935–0.935 | 12/12 |
| update-10000-dispersed / B/op | entry-validation/candidate | 0.880 | 0.880–0.880 | 12/12 |
| update-10000-dispersed / B/op | entry-validation/preallocated | 0.941 | 0.941–0.941 | 12/12 |
| update-10000-dispersed / allocs/op | preallocated/candidate | 0.992 | 0.992–0.992 | 12/12 |
| update-10000-dispersed / allocs/op | entry-validation/candidate | 0.977 | 0.977–0.977 | 12/12 |
| update-10000-dispersed / allocs/op | entry-validation/preallocated | 0.985 | 0.985–0.985 | 12/12 |
| update-10000-mixed / ns/op | preallocated/candidate | 0.995 | 0.979–1.001 | 9/12 |
| update-10000-mixed / ns/op | entry-validation/candidate | 0.937 | 0.932–0.955 | 12/12 |
| update-10000-mixed / ns/op | entry-validation/preallocated | 0.947 | 0.934–0.956 | 12/12 |
| update-10000-mixed / B/op | preallocated/candidate | 0.964 | 0.964–0.964 | 12/12 |
| update-10000-mixed / B/op | entry-validation/candidate | 0.934 | 0.934–0.934 | 12/12 |
| update-10000-mixed / B/op | entry-validation/preallocated | 0.969 | 0.969–0.969 | 12/12 |
| update-10000-mixed / allocs/op | preallocated/candidate | 0.996 | 0.996–0.996 | 12/12 |
| update-10000-mixed / allocs/op | entry-validation/candidate | 0.989 | 0.989–0.989 | 12/12 |
| update-10000-mixed / allocs/op | entry-validation/preallocated | 0.993 | 0.993–0.993 | 12/12 |

## btrfs-alternatives

Reference: original hierarchy-preserving candidate. Entry-validation includes preallocation in this campaign.

| Case / metric | Comparison | Paired ratio | Pair range | Wins |
|---|---|---:|---:|---:|
| update-1000-repeated / ns/op | preallocated/candidate | 0.912 | 0.744–1.165 | 10/12 |
| update-1000-repeated / ns/op | entry-validation/candidate | 0.784 | 0.525–1.022 | 11/12 |
| update-1000-repeated / ns/op | entry-validation/preallocated | 0.857 | 0.596–1.149 | 10/12 |
| update-1000-repeated / B/op | preallocated/candidate | 0.886 | 0.886–0.886 | 12/12 |
| update-1000-repeated / B/op | entry-validation/candidate | 0.884 | 0.884–0.884 | 12/12 |
| update-1000-repeated / B/op | entry-validation/preallocated | 0.998 | 0.998–0.998 | 12/12 |
| update-1000-repeated / allocs/op | preallocated/candidate | 0.708 | 0.708–0.708 | 12/12 |
| update-1000-repeated / allocs/op | entry-validation/candidate | 0.625 | 0.625–0.625 | 12/12 |
| update-1000-repeated / allocs/op | entry-validation/preallocated | 0.882 | 0.882–0.882 | 12/12 |
| update-1000-dispersed / ns/op | preallocated/candidate | 0.937 | 0.628–1.080 | 9/12 |
| update-1000-dispersed / ns/op | entry-validation/candidate | 0.831 | 0.532–1.096 | 10/12 |
| update-1000-dispersed / ns/op | entry-validation/preallocated | 0.883 | 0.732–1.074 | 8/12 |
| update-1000-dispersed / B/op | preallocated/candidate | 0.887 | 0.887–0.887 | 12/12 |
| update-1000-dispersed / B/op | entry-validation/candidate | 0.876 | 0.876–0.876 | 12/12 |
| update-1000-dispersed / B/op | entry-validation/preallocated | 0.987 | 0.987–0.987 | 12/12 |
| update-1000-dispersed / allocs/op | preallocated/candidate | 0.759 | 0.759–0.759 | 12/12 |
| update-1000-dispersed / allocs/op | entry-validation/candidate | 0.517 | 0.517–0.517 | 12/12 |
| update-1000-dispersed / allocs/op | entry-validation/preallocated | 0.682 | 0.682–0.682 | 12/12 |
| update-1000-mixed / ns/op | preallocated/candidate | 0.859 | 0.739–1.294 | 9/12 |
| update-1000-mixed / ns/op | entry-validation/candidate | 0.782 | 0.578–1.200 | 10/12 |
| update-1000-mixed / ns/op | entry-validation/preallocated | 0.888 | 0.744–1.062 | 11/12 |
| update-1000-mixed / B/op | preallocated/candidate | 0.887 | 0.887–0.887 | 12/12 |
| update-1000-mixed / B/op | entry-validation/candidate | 0.876 | 0.876–0.876 | 12/12 |
| update-1000-mixed / B/op | entry-validation/preallocated | 0.987 | 0.987–0.987 | 12/12 |
| update-1000-mixed / allocs/op | preallocated/candidate | 0.767 | 0.767–0.767 | 12/12 |
| update-1000-mixed / allocs/op | entry-validation/candidate | 0.533 | 0.533–0.533 | 12/12 |
| update-1000-mixed / allocs/op | entry-validation/preallocated | 0.696 | 0.696–0.696 | 12/12 |
| update-10000-repeated / ns/op | preallocated/candidate | 0.884 | 0.778–0.964 | 12/12 |
| update-10000-repeated / ns/op | entry-validation/candidate | 0.856 | 0.731–1.097 | 10/12 |
| update-10000-repeated / ns/op | entry-validation/preallocated | 0.986 | 0.787–1.158 | 8/12 |
| update-10000-repeated / B/op | preallocated/candidate | 0.802 | 0.802–0.802 | 12/12 |
| update-10000-repeated / B/op | entry-validation/candidate | 0.799 | 0.799–0.799 | 12/12 |
| update-10000-repeated / B/op | entry-validation/preallocated | 0.996 | 0.996–0.996 | 12/12 |
| update-10000-repeated / allocs/op | preallocated/candidate | 0.946 | 0.946–0.946 | 12/12 |
| update-10000-repeated / allocs/op | entry-validation/candidate | 0.930 | 0.930–0.930 | 12/12 |
| update-10000-repeated / allocs/op | entry-validation/preallocated | 0.984 | 0.984–0.984 | 12/12 |
| update-10000-dispersed / ns/op | preallocated/candidate | 0.981 | 0.847–1.082 | 8/12 |
| update-10000-dispersed / ns/op | entry-validation/candidate | 0.948 | 0.861–1.012 | 10/12 |
| update-10000-dispersed / ns/op | entry-validation/preallocated | 0.983 | 0.842–1.140 | 7/12 |
| update-10000-dispersed / B/op | preallocated/candidate | 0.935 | 0.934–0.935 | 12/12 |
| update-10000-dispersed / B/op | entry-validation/candidate | 0.880 | 0.879–0.881 | 12/12 |
| update-10000-dispersed / B/op | entry-validation/preallocated | 0.941 | 0.941–0.942 | 12/12 |
| update-10000-dispersed / allocs/op | preallocated/candidate | 0.992 | 0.990–0.993 | 12/12 |
| update-10000-dispersed / allocs/op | entry-validation/candidate | 0.977 | 0.976–0.978 | 12/12 |
| update-10000-dispersed / allocs/op | entry-validation/preallocated | 0.985 | 0.984–0.986 | 12/12 |
| update-10000-mixed / ns/op | preallocated/candidate | 0.992 | 0.770–1.196 | 8/12 |
| update-10000-mixed / ns/op | entry-validation/candidate | 0.909 | 0.738–1.019 | 11/12 |
| update-10000-mixed / ns/op | entry-validation/preallocated | 0.961 | 0.781–1.063 | 8/12 |
| update-10000-mixed / B/op | preallocated/candidate | 0.964 | 0.964–0.965 | 12/12 |
| update-10000-mixed / B/op | entry-validation/candidate | 0.934 | 0.934–0.935 | 12/12 |
| update-10000-mixed / B/op | entry-validation/preallocated | 0.969 | 0.968–0.970 | 12/12 |
| update-10000-mixed / allocs/op | preallocated/candidate | 0.996 | 0.995–0.997 | 12/12 |
| update-10000-mixed / allocs/op | entry-validation/candidate | 0.989 | 0.988–0.990 | 12/12 |
| update-10000-mixed / allocs/op | entry-validation/preallocated | 0.993 | 0.992–0.994 | 12/12 |

