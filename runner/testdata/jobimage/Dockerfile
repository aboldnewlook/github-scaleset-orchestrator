# Stand-in job image for runner/docker_integration_test.go. The real image's
# entrypoint is /usr/local/bin/gso-job; this one sleeps and exits as told.
FROM busybox:1.36
COPY gso-job /usr/local/bin/gso-job
RUN chmod 0755 /usr/local/bin/gso-job
