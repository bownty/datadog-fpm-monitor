job "{{PROJECT_NAME}}" {
    region      = "global"
    type        = "system"
    datacenters = ["production", "vagrant"]

    constraint {
        attribute = "${meta.kvm}"
        value     = "1"
    }

    task "server" {
        driver = "raw_exec"
        user   = "root"

        config {
            command = "datadog-fpm-monitor"
        }

        artifact {
            source = "https://storage.googleapis.com/bownty-deploy-artifacts/{{PROJECT_NAME}}/{{APP_ENV}}/{{APP_VERSION}}/datadog-fpm-monitor"
        }

        service {
            name = "{{PROJECT_NAME}}-go-expvar"
            check {
                type     = "tcp"
                port     = "http"
                interval = "10s"
                timeout  = "2s"
            }
        }

        resources {
            cpu    = 512
            memory = 256

            network {
                mbits = 1

                port "http" { }
            }
        }
    }
}