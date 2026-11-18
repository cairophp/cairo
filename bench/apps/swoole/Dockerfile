FROM phpswoole/swoole:6.2.5-php8.5
COPY php.ini /usr/local/etc/php/conf.d/zz-bench.ini
COPY . /app
CMD ["php", "/app/swoole/server.php"]
