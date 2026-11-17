FROM dunglas/frankenphp:1.13.5-php8.5.12
RUN install-php-extensions pdo_pgsql pdo_mysql
COPY frankenphp/Caddyfile /etc/frankenphp/Caddyfile
COPY php.ini /usr/local/etc/php/conf.d/zz-bench.ini
COPY . /app
