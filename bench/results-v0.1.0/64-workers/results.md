Requests per second, median of the runs, with the p99 latency in brackets.

| Case | nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner |
|---|---:|---:|---:|
| hello | 12,761 (40.0 ms) | 16,874 (10.3 ms) | 11,997 (17.3 ms) |
| cpu | 3,284 (65.0 ms) | 3,099 (37.3 ms) | 3,705 (41.4 ms) |
| sleep | 3,075 (89.9 ms) | 3,163 (86.5 ms) | 3,050 (94.8 ms) |
| curl | 2,879 (124.2 ms) | 3,001 (100.0 ms) | 2,865 (105.3 ms) |
| pgsql-slow | 5,416 (73.0 ms) | 6,010 (47.7 ms) | 5,703 (51.5 ms) |
| mysql-slow | 5,166 (83.0 ms) | 5,943 (65.2 ms) | 5,974 (55.0 ms) |

Resident memory after the `sleep` case (MiB):

| nginx + PHP-FPM | FrankenPHP (worker) | RoadRunner |
|---:|---:|---:|
| 66 | 125 | 1548 |
