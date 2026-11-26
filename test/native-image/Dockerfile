# What CI builds on the native image (Dockerfile.native): an app's image that
# adds an extension with the base image's own tool, as its docs show.
ARG BASE=cairo-native
FROM ${BASE}
USER root
RUN docker-php-ext-install shmop
USER cairo
COPY server.php /app/server.php
