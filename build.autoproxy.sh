#!/bin/bash
set -e

# ensure that docker-autoproxy exists in the local directory before allowing
# the script to continue
if [ ! -f docker-autoproxy ]; then
    echo "ERROR: docker-autoproxy not found!"
    echo "ERROR: did you build the Go application first?"
    exit 1
fi

# build autoproxy docker container
docker build -t "autoproxy" -f Dockerfile.autoproxy .
