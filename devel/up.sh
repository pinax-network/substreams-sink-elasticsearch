#!/usr/bin/env bash

set -e

ROOT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && cd .. && pwd )"

clean=

main() {
  pushd "$ROOT" &> /dev/null

  while getopts "hc" opt; do
    case $opt in
      h) usage && exit 0;;
      c) clean=true;;
      \?) usage_error "Invalid option: -$OPTARG";;
    esac
  done
  shift $((OPTIND-1))

  if [[ -d "./devel/data" && $clean == true ]]; then
    echo "Cleaning data directory"
    rm -rf ./devel/data 1> /dev/null
  fi

  prepare

  # Pass execution to docker compose
  exec docker-compose up
}

prepare() {
  if [[ ! -d "./devel/data/elastic" ]]; then
    mkdir -p ./devel/data/elastic 1> /dev/null
  fi
}

usage_error() {
  message="$1"
  exit_code="$2"

  echo "ERROR: $message"
  echo ""
  usage
  exit ${exit_code:-1}
}

usage() {
  echo "usage: up [-c]"
  echo ""
  echo "Setup required files layout and launch 'docker compose up'"
  echo "spinning up all required development dependencies."
  echo ""
  echo "Options"
  echo "    -c          Clean 'data' directory before launching dependencies"
  echo "    -h          Display help about this script"
}

main "$@"


