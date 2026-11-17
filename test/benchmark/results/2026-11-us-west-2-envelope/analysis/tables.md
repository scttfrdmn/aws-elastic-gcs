# Envelope study tables (steady_gbps)

Mean over repetitions with a 95% t-interval (ci_kind=reps); single runs show the within-run interval of the measurement window (within).

## P1:c:0:304e75

Factor `c`; fixed: `{"dataset": "F256M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.601 | [0.345, 0.857] | - | within |
| 2 | 1 | 1.306 | [1.256, 1.356] | - | within |
| 4 | 1 | 2.540 | [2.249, 2.831] | - | within |
| 8 | 1 | 5.437 | [4.935, 5.939] | - | within |

## P1:c:0:3a9206

Factor `c`; fixed: `{"dataset": "F4G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 2 | 0.648 | [-0.032, 1.328] | 0.117 | reps |
| 2 | 1 | 1.113 | [0.962, 1.264] | - | within |
| 4 | 1 | 2.323 | [1.872, 2.774] | - | within |
| 8 | 1 | 5.035 | [3.658, 6.411] | - | within |

## P1:c:0:3e633e

Factor `c`; fixed: `{"dataset": "F16G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.962 | [0.947, 0.977] | - | within |
| 2 | 1 | 1.924 | [1.888, 1.960] | - | within |
| 4 | 1 | 3.837 | [3.765, 3.909] | - | within |
| 8 | 1 | 5.979 | [5.971, 5.988] | - | within |

## P1:c:0:453c6c

Factor `c`; fixed: `{"dataset": "F1M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.051 | [0.045, 0.057] | - | within |
| 2 | 1 | 0.080 | [0.038, 0.122] | - | within |
| 4 | 1 | 0.183 | [-0.106, 0.472] | - | within |
| 8 | 1 | 0.431 | [-0.162, 1.025] | - | within |

## P1:c:0:4735a8

Factor `c`; fixed: `{"dataset": "F16G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 2 | 0.500 | [-0.594, 1.594] | 0.243 | reps |
| 2 | 1 | 0.948 | [0.868, 1.028] | - | within |
| 4 | 2 | 2.142 | [-0.782, 5.066] | 0.152 | reps |
| 8 | 2 | 4.942 | [3.675, 6.210] | 0.029 | reps |

## P1:c:0:4f9dfa

Factor `c`; fixed: `{"dataset": "F1G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 3 | 0.588 | [0.377, 0.799] | 0.144 | reps |
| 2 | 1 | 1.459 | [1.321, 1.597] | - | within |
| 4 | 1 | 3.056 | [2.946, 3.167] | - | within |
| 8 | 3 | 5.676 | [4.637, 6.714] | 0.074 | reps |

## P1:c:0:5c69f9

Factor `c`; fixed: `{"dataset": "F256M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.935 | [0.910, 0.960] | - | within |
| 2 | 1 | 1.861 | [1.831, 1.891] | - | within |
| 4 | 1 | 3.706 | [3.639, 3.772] | - | within |
| 8 | 1 | 5.986 | [5.979, 5.994] | - | within |

## P1:c:0:5dcf0f

Factor `c`; fixed: `{"dataset": "F64M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.832 | [0.794, 0.869] | - | within |
| 2 | 1 | 1.634 | [1.574, 1.694] | - | within |
| 4 | 1 | 3.310 | [3.244, 3.375] | - | within |
| 8 | 1 | 5.489 | [4.591, 6.387] | - | within |

## P1:c:0:6c0ea0

Factor `c`; fixed: `{"dataset": "F1M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.065 | [0.063, 0.068] | - | within |
| 2 | 2 | 0.123 | [0.067, 0.178] | 0.050 | reps |
| 4 | 1 | 0.195 | [-0.149, 0.538] | - | within |
| 8 | 1 | 0.536 | [0.528, 0.543] | - | within |

## P1:c:0:6f6529

Factor `c`; fixed: `{"dataset": "F4G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.839 | [0.240, 1.437] | - | within |
| 2 | 1 | 1.547 | [-4.896, 7.991] | - | within |
| 4 | 1 | 2.477 | [-13.641, 18.595] | - | within |
| 8 | 1 | 4.746 | [-16.235, 25.727] | - | within |

## P1:c:0:70b43f

Factor `c`; fixed: `{"dataset": "F16G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.662 | [0.180, 1.144] | - | within |
| 2 | 1 | 1.254 | [1.152, 1.357] | - | within |
| 4 | 1 | 2.665 | [2.418, 2.912] | - | within |
| 8 | 1 | 5.277 | [5.053, 5.501] | - | within |

## P1:c:0:752120

Factor `c`; fixed: `{"dataset": "F16M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.562 | [0.541, 0.583] | - | within |
| 2 | 1 | 1.200 | [1.182, 1.218] | - | within |
| 4 | 1 | 2.201 | [1.942, 2.460] | - | within |
| 8 | 2 | 4.284 | [0.396, 8.172] | 0.101 | reps |

## P1:c:0:8ad63b

Factor `c`; fixed: `{"dataset": "F256M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.680 | [0.576, 0.785] | - | within |
| 2 | 1 | 1.123 | [0.366, 1.881] | - | within |
| 4 | 1 | 2.642 | [2.183, 3.102] | - | within |
| 8 | 1 | 4.588 | [4.292, 4.884] | - | within |

## P1:c:0:8c9fe0

Factor `c`; fixed: `{"dataset": "F16M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.429 | [0.354, 0.503] | - | within |
| 2 | 1 | 1.072 | [1.034, 1.110] | - | within |
| 4 | 1 | 1.896 | [1.805, 1.986] | - | within |
| 8 | 1 | 3.498 | [-0.922, 7.918] | - | within |

## P1:c:0:8ed40d

Factor `c`; fixed: `{"dataset": "F1G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 3 | 0.552 | [0.252, 0.852] | 0.219 | reps |
| 2 | 1 | 1.377 | [0.945, 1.810] | - | within |
| 4 | 1 | 2.710 | [0.411, 5.010] | - | within |
| 8 | 3 | 5.548 | [3.731, 7.365] | 0.132 | reps |

## P1:c:0:940d73

Factor `c`; fixed: `{"dataset": "F16M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.549 | [0.518, 0.580] | - | within |
| 2 | 1 | 1.273 | [1.208, 1.338] | - | within |
| 4 | 2 | 2.097 | [0.143, 4.051] | 0.104 | reps |
| 8 | 1 | 3.457 | [1.014, 5.900] | - | within |

## P1:c:0:9ebed3

Factor `c`; fixed: `{"dataset": "F64M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.609 | [0.555, 0.664] | - | within |
| 2 | 1 | 1.251 | [1.141, 1.360] | - | within |
| 4 | 1 | 2.365 | [2.196, 2.534] | - | within |
| 8 | 1 | 4.277 | [1.343, 7.211] | - | within |

## P1:c:0:b400cf

Factor `c`; fixed: `{"dataset": "F1M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 2 | 0.070 | [0.026, 0.115] | 0.070 | reps |
| 2 | 1 | 0.157 | [0.150, 0.165] | - | within |
| 4 | 1 | 0.224 | [0.160, 0.288] | - | within |
| 8 | 1 | 0.604 | [0.585, 0.623] | - | within |

## P1:c:0:c55491

Factor `c`; fixed: `{"dataset": "F4G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_posix", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.602 | [0.565, 0.640] | - | within |
| 2 | 1 | 0.800 | [-2.497, 4.098] | - | within |
| 4 | 1 | 2.509 | [1.552, 3.466] | - | within |
| 8 | 1 | 4.377 | [2.037, 6.717] | - | within |

## P1:c:0:e515e9

Factor `c`; fixed: `{"dataset": "F64M", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "s3_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 1 | 0.593 | [0.534, 0.652] | - | within |
| 2 | 1 | 1.236 | [1.162, 1.311] | - | within |
| 4 | 1 | 2.692 | [2.545, 2.838] | - | within |
| 8 | 1 | 4.170 | [4.051, 4.289] | - | within |

## P1:c:0:fe8125

Factor `c`; fixed: `{"dataset": "F1G", "dst_fanout": null, "encrypt_data": false, "instance_type_dst": "c8in.2xlarge", "instance_type_src": "c8in.2xlarge", "k": 1, "loss_pct": 0, "n_dst": 1, "n_src": 1, "network_use": "custom", "p": 4, "route": "posix_to_s3", "rtt_ms": 0, "verify_checksum": true}`

| c | n | mean | 95% CI | CV | CI kind |
|---|---|---|---|---|---|
| 1 | 2 | 0.938 | [0.905, 0.971] | 0.004 | reps |
| 2 | 1 | 1.875 | [1.676, 2.075] | - | within |
| 4 | 1 | 3.785 | [3.412, 4.157] | - | within |
| 8 | 3 | 5.998 | [5.937, 6.059] | 0.004 | reps |
