![License MIT](https://img.shields.io/badge/license-MIT-blue.svg)

docker-autoproxy sets up a container running nginx and and a small companion
application written in Go. The companion app generates reverse proxy
configurations for nginx and reloads nginx when containers are started and
stopped.

This project is heavily inspired by (and lifts part of this README from)
[nginx-proxy](https://github.com/jwilder/nginx-proxy) but has a slightly
different (much smaller) feature set. nginx-proxy is a much more generic
solution and if docker-autoproxy doesn't meet your needs you should definitely
check it out.


### Usage

To run it:

```bash
# note that this doesn't actually work yet, but will once i push a release
# to the registry, for now you can just build manually following the
# instructions below.
$ docker run -d -p 80:80 -p 443:443 -v /var/run/docker.sock:/var/run/docker.sock rehabstudio/docker-autoproxy
```

Then start any containers you want proxied with an env var
`VIRTUAL_HOST=subdomain.youdomain.com`

```bash
$ docker run -e VIRTUAL_HOST=foo.bar.com  ...
```

Provided your DNS is setup to forward foo.bar.com to the a host running
`docker-autoproxy`, the request will be routed to a container with the
VIRTUAL_HOST env var set.


### Multiple Ports

If your container exposes multiple ports, you need to specify a which port
should be used. You can set a VIRTUAL_PORT env var to select the correct port.
If your container only exposes one port and it has a VIRTUAL_HOST env var set,
that port will be selected automatically.

```bash
$ docker run -e VIRTUAL_HOST=foo.bar.com -e VIRTUAL_PORT=80 nginx:latest
```

### Wildcard Hosts

You can also use wildcards at the beginning and the end of host name, like
`*.bar.com` or `foo.bar.*`. Or even a regular expression, which can be very
useful in conjunction with a wildcard DNS service like [xip.io](http://xip.io),
using `~^foo\.bar\..*\.xip\.io` will match `foo.bar.127.0.0.1.xip.io`,
`foo.bar.10.0.2.2.xip.io` and all other given IPs. More information about this
topic can be found in the
[nginx documentation](http://nginx.org/en/docs/http/server_names.html).


### SSL Support

SSL is supported using single host, wildcard and SNI certificates by specifying
a cert name as the environment variable `SSL_CERT_NAME`.

To enable SSL:

```bash
$ docker run -d -p 80:80 -p 443:443 -v /path/to/certs:/etc/nginx/ssl.d -v /var/run/docker.sock:/tmp/docker.sock rehabstudio/docker-autoproxy
```

The contents of `/path/to/certs` should contain the certificates and private
keys for any virtual hosts in use.  The certificate and keys should be named
after the `SSL_CERT_NAME` env var with a `.crt` and `.key` extension.  For
example, a container with `SSL_CERT_NAME=foobar` should have a `foobar.crt` and
`foobar.key` file in the certs directory.


### Basic Authentication Support

`docker-autoproxy` supports HTTP basic authentication using the standard
apache/nginx htpasswd format. Users should pass a JSON encoded array of
username/password pairs using the `HTPASSWD` environment variable to each
container that should be secured.

```bash
$ docker run -e "HTPASSWD=[\"auser:$apr1$SFAk1m9U...\", \"anotheruser:$apr1$7uFp./y...\"]" ...
```

The format of this variable is a bit awkward, but is much easier to manage
using other tools (like fig, or oneill) than the docker CLI directly. To
generate a password in the appropriate format you can follow these
[instructions](http://httpd.apache.org/docs/2.2/programs/htpasswd.html).


### Building autoproxy locally

If you're planning to customise autoproxy, whether to submit a patch or just to
customise a private build, the easiest way to do so is to clone this repo
locally, build a private copy and push to a docker registry.

This repository contains a simple build script that depends only on Docker and
Bash. The application is built in multiple steps, using several containers
throughout the process. First the Go application is tested and built inside a
docker container, upon completion the newly built binary is saved to the
current folder on the host system. Next, the application binary and
configuration template are installed in a new container alongside the latest
version of nginx to produce the final docker-autoproxy build.

```bash
$ ./build.sh
```

Some of the reasons you may want to use a custom build are:

- To bake your SSL certificates into the image so that you're not relying on
  mounting a host volume at runtime.
- Modifying the nginx configuration template to provide different behavior.
