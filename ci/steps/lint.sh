#!/bin/bash

echo "Linting..."

go vet ./...
golint -set_exit_status ./...