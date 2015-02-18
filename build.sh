#!/bin/bash
set -e

# build go binary inside a docker container
./build.go.sh docker
# build autoproxy docker container
./build.autoproxy.sh
