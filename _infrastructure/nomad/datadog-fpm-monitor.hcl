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