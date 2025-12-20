#!/bin/bash
echo "Starting SatoshiSend at http://localhost:8080"
go run ./cmd/server -addr :8080 -dev
