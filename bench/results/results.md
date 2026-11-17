Requests per second, median of the runs, with the p99 latency in brackets.

| Case | Go net/http | Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---|---:|---:|---:|---:|---:|---:|
| hello | 99,085 (3.1 ms) | 42,322 (7.4 ms) | 13,798 (18.6 ms) | 18,144 (9.6 ms) | 13,604 (14.0 ms) | 121,563 (1.3 ms) |
| json | – | 40,633 (6.8 ms) | 11,248 (16.8 ms) | 16,897 (10.3 ms) | 13,443 (14.2 ms) | 119,565 (1.3 ms) |
| cpu | – | 3,895 (22.8 ms) | 3,146 (43.2 ms) | 3,174 (37.0 ms) | 3,808 (32.4 ms) | 3,801 (40.1 ms) |
| sleep | – | 11,227 (39.0 ms) | 1,921 (140.1 ms) | 1,967 (139.3 ms) | 1,901 (142.5 ms) | 11,981 (23.6 ms) |
| curl | – | 11,612 (31.4 ms) | 1,781 (160.9 ms) | 1,834 (151.9 ms) | 1,773 (156.0 ms) | 9,113 (49.8 ms) |
| pgsql | – | 12,377 (11.3 ms) | 6,357 (16.9 ms) | 7,622 (19.0 ms) | 6,367 (20.2 ms) | 9,246 (21.4 ms) |
| pgsql-slow | – | 3,620 (168.9 ms) | 3,643 (106.1 ms) | 3,750 (79.8 ms) | 3,528 (108.7 ms) | 3,607 (114.0 ms) |
| mysql | – | 13,461 (10.7 ms) | 7,901 (14.2 ms) | 9,610 (16.0 ms) | 9,014 (16.2 ms) | 24,803 (5.7 ms) |
| mysql-slow | – | 3,666 (89.4 ms) | 3,632 (103.0 ms) | 3,764 (92.1 ms) | 3,522 (93.8 ms) | 3,734 (92.2 ms) |
| grpc-unary | – | 15,926 (9.1 ms) | – | – | 15,430 (9.8 ms) ⚠ 31 errors | – |
| client | – | 11,351 (33.2 ms) | – | – | – | – |
| sql-pgsql | – | 15,374 (10.4 ms) | – | – | – | – |
| sql-mysql | – | 15,425 (9.6 ms) | – | – | – | – |
| grpc-stream-100 | – | 886 (144.4 ms) | – | – | – | – |
| grpc-slow-20ms | – | 2,778 (29.8 ms) | – | – | – | – |

Resident memory after the `sleep` case (MiB):

| Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---:|---:|---:|---:|---:|
| 100 | 50 | 96 | 975 | 26 |
