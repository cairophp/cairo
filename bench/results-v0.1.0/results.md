Requests per second, median of the runs, with the p99 latency in brackets.

| Case | Go net/http | Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---|---:|---:|---:|---:|---:|---:|
| hello | 84,186 (4.8 ms) | 19,481 (21.4 ms) | 13,837 (6.6 ms) | 19,223 (9.2 ms) | 18,936 (6.8 ms) | 118,562 (3.4 ms) |
| json | – | 19,231 (20.1 ms) | 11,913 (7.5 ms) | 16,909 (10.3 ms) | 16,557 (7.2 ms) | 118,901 (1.2 ms) |
| cpu | – | 3,698 (23.0 ms) | 3,389 (25.4 ms) | 3,111 (40.5 ms) | 3,647 (25.3 ms) | 3,705 (46.9 ms) |
| sleep | – | 11,570 (39.6 ms) | 743 (472.5 ms) | 780 (336.0 ms) | 733 (522.2 ms) | 11,992 (23.7 ms) |
| curl | – | 5,973 (60.6 ms) | 689 (511.6 ms) | 737 (355.7 ms) | 700 (438.2 ms) | 11,103 (39.0 ms) |
| client | – | 8,043 (56.2 ms) | – | – | – | – |
| pgsql | – | 8,089 (19.3 ms) ⚠ 7 errors | 7,527 (12.8 ms) | 8,016 (18.4 ms) | 7,050 (14.3 ms) | 12,118 (10.1 ms) |
| pgsql-slow | – | 3,630 (98.7 ms) ⚠ 53 errors | 1,281 (231.0 ms) | 1,381 (199.6 ms) | 1,268 (241.1 ms) | 3,677 (84.7 ms) |
| sql-pgsql | – | 9,023 (18.2 ms) | – | – | – | – |
| mysql | – | 7,131 (21.7 ms) ⚠ 8 errors | 7,823 (12.4 ms) | 10,418 (15.0 ms) | 10,383 (10.1 ms) | 23,693 (6.2 ms) |
| mysql-slow | – | 3,722 (95.9 ms) ⚠ 26 errors | 1,333 (220.8 ms) | 1,454 (181.8 ms) | 1,376 (202.0 ms) | 3,729 (100.6 ms) |
| sql-mysql | – | 6,854 (19.3 ms) | – | – | – | – |
| grpc-unary | – | 12,979 (10.5 ms) | – | – | 14,632 (10.4 ms) ⚠ 26 errors | – |
| grpc-stream-100 | – | 444 (442.4 ms) | – | – | – | – |
| grpc-slow-20ms | – | 2,753 (32.8 ms) | – | – | – | – |

Resident memory after the `sleep` case (MiB):

| Cairo | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner | Swoole |
|---:|---:|---:|---:|---:|
| 71 | 31 | 69 | 412 | 28 |
