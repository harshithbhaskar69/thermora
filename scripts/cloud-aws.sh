#!/bin/sh
set -eu

project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
export AWS_CONFIG_FILE="$project_dir/deploy/cloud/aws.config"
export AWS_SHARED_CREDENTIALS_FILE=/dev/null
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_SECURITY_TOKEN
unset AWS_REGION AWS_DEFAULT_REGION
export AWS_EC2_METADATA_DISABLED=true AWS_CLI_AUTO_PROMPT=off AWS_PAGER=
export NO_PROXY=127.0.0.1 no_proxy=127.0.0.1

# Fixed final options take precedence over ambient endpoints and caller options.
# The explicit profile uses only the dummy credentials in the project config.
exec aws "$@" --profile floci --endpoint-url http://127.0.0.1:4566
