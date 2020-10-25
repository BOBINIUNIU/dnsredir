/**
 * Created: Oct 25, 2020.
 * License: MIT.
 */

#pragma once

#include <stdint.h>

int pf_add_addr(int, const char *, const char *, const uint8_t *, size_t);
int pf_add_table(int, const char *, const char *);
int open_dev_pf(int);
int close_dev_pf(int);
