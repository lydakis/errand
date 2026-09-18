# Apply follow-up

Individual operation medians in milliseconds. A/B run the identical reference binary; modes rotate within each round.

| Case | reference-a | reference-b | scratch | parents |
|---|---:|---:|---:|---:|
| tiny | 84.013 | 81.565 | 80.121 | 79.647 |
| large | 126.942 | 127.852 | 124.400 | 123.622 |
| flat128 | 3205.640 | 2849.904 | 3203.692 | 3009.927 |
| parents8 | 5694.771 | 6123.933 | 5044.577 | 2519.529 |
| parents128 | 5651.294 | 6182.633 | 5315.204 | 2980.098 |
| creation | 6176.702 | 5624.313 | 5734.950 | 5520.078 |
| deletion | 6233.488 | 5587.445 | 5745.470 | 5355.172 |
| new-parent | 6652.314 | 5617.331 | 5802.001 | 5201.815 |
| restricted | 5933.630 | 6488.802 | 5312.914 | 5532.997 |
| watch-small | 217.406 | 219.121 | 216.467 | 215.172 |
