#!/bin/bash

set -e

fig run db createdb -h db -U postgres firesize_development
fig run db createdb -h db -U postgres firesize_test
fig run web goose up
