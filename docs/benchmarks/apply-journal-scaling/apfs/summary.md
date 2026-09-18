# Apply-journal scaling

One complete `TransferTarget.Apply` per sample, including durable receipt and cleanup. Fixed 1 MiB changed; flat, existing-parent file replacements. Setup and retry checks are outside timing.

| Roots | Plain A median ms | Plain B median ms | Instrumented median ms | Sync ms | Validate + encode ms | Journal bytes | Sync calls |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 130.82 | 118.10 | 131.23 | 91.52 | 0.22 | 3,474.00 | 26.00 |
| 8 | 451.26 | 447.05 | 449.13 | 346.63 | 1.73 | 83,729.00 | 110.00 |
| 32 | 1,595.65 | 1,553.23 | 1,561.83 | 1,275.74 | 16.74 | 1,162,409.00 | 398.00 |
| 128 | 5,778.63 | 5,641.31 | 5,821.46 | 4,733.82 | 221.62 | 17,918,729.00 | 1,550.00 |
| 512 | 25,937.09 | 25,636.19 | 26,738.78 | 19,779.88 | 3,464.85 | 284,009,609.00 | 6,158.00 |

Synchronization times are sums of measured `File.Sync` calls. Component spans overlap: journal publication includes its validation, encoding and barriers; core apply includes journal publication. Do not add inclusive spans together. Instrumentation adds clocks and accounting, so use plain samples for latency. A and B execute the identical binary as an execution-position control.
