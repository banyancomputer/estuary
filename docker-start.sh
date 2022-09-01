#/bin/bash

'''
This is the script that is run when the docker container is started.
Please see the Dockerfile for more information on the context of this script.
'''

# This is the WORKDIR of the docker container.
WORKDIR=/app

FULLNODE_API=$FULLNODE_API
ESTUARY_HOST=$ESTUARY_HOST
ESTUARY_PORT=$ESTUARY_PORT

if [ -z "$FULLNODE_API" ]; then
    echo "FULLNODE_API is empty, use default value"
    FULLNODE_API="wss://api.chain.love"
fi

echo "HOSTNAME: $ESTUARY_HOSTNAME:$ESTUARY_PORT"
echo "FULLNODE_API: $FULLNODE_API"


# if test -f "$FILE"; then
#     echo "$FILE exists."
#     /usr/src/estuary/estuary --hostname $ESTUARY_HOSTNAME
# else
#     echo "$FILE does not exist."
#     mkdir -p /usr/src/estuary/data
#     # note (al): Pretty sure this has changed since our last commit
#     # AUTH_KEY=$(/usr/src/estuary/estuary setup --username admin --password Password123 | grep Token | cut -d ' ' -f 3)
#     AUTH_KEY=$(/usr/src/estuary/estuary setup --username=admin | grep Token | cut -d ' ' -f 3)
#     echo $AUTH_KEY
#     echo $AUTH_KEY > /usr/estuary/private/token
#     cat /usr/estuary/private/token
# fi

# Setup a new node and record the Estuary Token
mkdir -p $WORKDIR/data
mkdir -p $WORKDIR/private
ESTUARY_TOKEN=$($WORKDIR/estuary setup --username=admin | grep Token | cut -d ' ' -f 3)
echo $ESTUARY_TOKEN > $WORKDIR/private/token
echo "Estuary Admin Key: $ESTUARY_TOKEN"

$WORKDIR/estuary $ESTUARY_HOSTNAME