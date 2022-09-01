# syntax=docker/dockerfile:1

# ## Build
FROM golang:1.18-bullseye

# Default Arguments for the Build
# TODO: Figure out why this arg isn't working
ARG arg_repo_branch=docker
ARG arg_estuary_host=estuary-main
ARG arg_estuary_port=3004
ARG arg_fullnode_api=ws://api.chain.love
# TODO: Make this configurable
ARG arg_estuary_www_host=localhost
ARG arg_estuary_www_port=4443

# Env Variables for the Builds

# Estuary Hostname and Port
ENV ESTUARY_HOST=$arg_estuary_hostname 
ENV ESTUARY_PORT=$arg_estuary_port
# Fullnode API (this is a Lotus node)
ENV FULLNODE_API=$arg_fullnode_api
 # Estuary Token (this is generated in docker-start.sh)
ENV ESTUARY_TOKEN=<>
# Estuary WWW Host and Port
ENV ESTUARY_WWW_HOST=$arg_estuary_www_host
ENV ESTUARY_WWW_PORT=$arg_estuary_www_port

# Create build directory
WORKDIR /app

# Install and Configure External Dependencies
RUN apt-get update && \
    apt-get install -y wget jq hwloc ocl-icd-opencl-dev git libhwloc-dev pkg-config make  && \
    apt-get install -y cargo
RUN curl https://sh.rustup.rs -sSf | bash -s -- -y
ENV PATH="/root/.cargo/bin:${PATH}"
RUN cargo --help

# Copy our Source Code from the Git Repository, Download Dependencies, and Compile
RUN git clone -b docker https://github.com/banyancomputer/estuary . && \
    RUSTFLAGS="-C target-cpu=native -g" FFI_BUILD_FROM_SOURCE=1 make all
RUN chmod +x /app/docker-start.sh

EXPOSE 3004

CMD /app/docker-start.sh
