CC ?= cc
CFLAGS ?= -O2 -Wall -Wextra
LDLIBS = -lcurl

artifactd-cli: artifactd_client.c
	$(CC) $(CFLAGS) -o $@ $< $(LDLIBS)

clean:
	rm -f artifactd-cli
.PHONY: clean
