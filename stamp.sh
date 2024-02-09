#!/bin/bash

echo VERSION $(git rev-parse --short HEAD || echo 'development')
echo BUILD_TIME $(date +"%Y-%m-%d:T%H:%M:%S")
