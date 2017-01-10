# datadog-service-helper

The purpose of this tool is to easy datadog service monitoring in a docker environment where services change port and host all the time.

## Current service backends

### php-fpm

- `PHP_FPM_CONFIG_FILE` (default: `/etc/dd-agent/conf.d/php_fpm.yaml`) path to the dd-agent `php_fpm.yaml` file.

### go_expr

- `GO_EXPR_TARGET_FILE` (default: `/etc/dd-agent/conf.d/go_expvar.yaml`) path to the dd-agent `go_expr.yaml` file.

### redis

- `REDIS_TARGET_FILE` (default: `/etc/dd-agent/conf.d/redis.yaml`) path to the dd-agent `redis.yaml` file.

## Local development

To get the dependencies and first build, please run:

```
make install
```

For easy Local development run

```
go install && \
    DONT_RELOAD_DATADOG=1 \
    TARGET_FILE_GO_EXPR=go_expr.yaml \
    TARGET_FILE_PHP_FPM=php_fpm.yaml \
    CONSUL_HTTP_ADDR=<consul client address>:8500 \
    datadog-fpm-monitor
```