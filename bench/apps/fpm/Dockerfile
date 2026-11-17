FROM php:8.5.12-fpm
COPY --from=mlocati/php-extension-installer:latest /usr/bin/install-php-extensions /usr/local/bin/
RUN install-php-extensions pdo_pgsql pdo_mysql
COPY fpm/php-fpm.conf /usr/local/etc/php-fpm.d/zz-bench.conf
COPY php.ini /usr/local/etc/php/conf.d/zz-bench.ini
COPY . /app
