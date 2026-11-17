Requests per second, median of the runs, with the p99 latency in brackets.

| Case | Go net/http | Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---|---:|---:|---:|---:|---:|---:|
| hello | 99,085 (3.1 ms) | 36,314 (8.2 ms) | 13,798 (18.6 ms) | 18,144 (9.6 ms) | 13,604 (14.0 ms) | 121,563 (1.3 ms) |
| json | – | 35,120 (8.0 ms) | 11,248 (16.8 ms) | 16,897 (10.3 ms) | 13,443 (14.2 ms) | 119,565 (1.3 ms) |
| cpu | – | 3,901 (23.0 ms) | 3,146 (43.2 ms) | 3,174 (37.0 ms) | 3,808 (32.4 ms) | 3,801 (40.1 ms) |
| sleep | – | 11,023 (45.3 ms) | 1,921 (140.1 ms) | 1,967 (139.3 ms) | 1,901 (142.5 ms) | 11,981 (23.6 ms) |
| curl | – | 11,430 (38.5 ms) | 1,781 (160.9 ms) | 1,834 (151.9 ms) | 1,773 (156.0 ms) | 9,113 (49.8 ms) |
| client | – | 10,631 (40.3 ms) | – | – | – | – |
| pgsql | – | 11,682 (13.2 ms) | 6,357 (16.9 ms) | 7,622 (19.0 ms) | 6,367 (20.2 ms) | 9,246 (21.4 ms) |
| pgsql-slow | – | 3,567 (162.5 ms) | 3,643 (106.1 ms) | 3,750 (79.8 ms) | 3,528 (108.7 ms) | 3,607 (114.0 ms) |
| sql-pgsql | – | 13,875 (12.9 ms) | – | – | – | – |
| mysql | – | 12,881 (11.7 ms) | 7,901 (14.2 ms) | 9,610 (16.0 ms) | 9,014 (16.2 ms) | 24,803 (5.7 ms) |
| mysql-slow | – | 3,679 (81.5 ms) | 3,632 (103.0 ms) | 3,764 (92.1 ms) | 3,522 (93.8 ms) | 3,734 (92.2 ms) |
| sql-mysql | – | 9,459 (15.8 ms) | – | – | – | – |
| grpc-unary | – | 15,075 (9.7 ms) | – | – | 15,430 (9.8 ms) ⚠ 31 errors | – |
| grpc-stream-100 | – | 820 (163.2 ms) | – | – | – | – |
| grpc-slow-20ms | – | 2,745 (31.8 ms) | – | – | – | – |

Resident memory after the `sleep` case (MiB):

| Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---:|---:|---:|---:|---:|
| 76 | 50 | 96 | 975 | 26 |
