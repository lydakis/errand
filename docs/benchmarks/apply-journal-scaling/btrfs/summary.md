# Apply-journal scaling

One complete `TransferTarget.Apply` per sample, including durable receipt and cleanup. Fixed 1 MiB changed; flat, existing-parent file replacements. Setup and retry checks are outside timing.

| Roots | Plain A median ms | Plain B median ms | Instrumented median ms | Sync ms | Validate + encode ms | Journal bytes | Sync calls |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1 | 120.82 | 123.69 | 125.11 | 75.95 | 0.16 | 3,432.00 | 26.00 |
| 8 | 460.46 | 523.10 | 378.70 | 309.29 | 1.10 | 83,141.00 | 110.00 |
| 32 | 1,329.40 | 1,482.02 | 1,279.95 | 1,126.68 | 10.46 | 1,155,485.00 | 398.00 |
| 128 | 5,545.70 | 5,195.02 | 5,440.09 | 4,846.64 | 133.59 | 17,817,341.00 | 1,550.00 |
| 512 | 24,776.14 | 24,705.97 | 24,477.45 | 20,803.85 | 2,046.71 | 282,424,445.00 | 6,158.00 |

Synchronization times are sums of measured `File.Sync` calls. Component spans overlap: journal publication includes its validation, encoding and barriers; core apply includes journal publication. Do not add inclusive spans together. Instrumentation adds clocks and accounting, so use plain samples for latency. A and B execute the identical binary as an execution-position control.
