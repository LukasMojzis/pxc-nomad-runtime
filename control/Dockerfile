FROM python:3.12-alpine

ARG NOMAD_VERSION=2.0.3
RUN apk add --no-cache curl unzip ca-certificates \
  && curl -fsSLo /tmp/nomad.zip "https://releases.hashicorp.com/nomad/${NOMAD_VERSION}/nomad_${NOMAD_VERSION}_linux_amd64.zip" \
  && unzip /tmp/nomad.zip -d /usr/local/bin \
  && rm -f /tmp/nomad.zip \
  && chmod 0755 /usr/local/bin/nomad

COPY pxc_control.py /usr/local/bin/pxc-control
RUN chmod 0755 /usr/local/bin/pxc-control

EXPOSE 8080
ENTRYPOINT ["pxc-control"]
