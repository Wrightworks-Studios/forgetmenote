// Preloaded into the browser-test Chromium on Linux only (see no-netlink.mjs).
//
// Chromium follows host address changes over a NETLINK_ROUTE socket and fails
// pending requests with net::ERR_NETWORK_CHANGED when one arrives. Release
// verification starts and removes Docker containers beside the browser suite;
// each container adds and removes host interfaces, so fixture page loads lost
// module scripts and pages never initialised. Refusing that one socket makes
// Chromium keep its "always online" default and ignore unrelated interfaces.
// Every other socket, including loopback HTTP to the fixtures, is untouched.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <linux/netlink.h>
#include <sys/socket.h>

int socket(int domain, int type, int protocol) {
  static int (*next)(int, int, int);
  if (domain == AF_NETLINK && protocol == NETLINK_ROUTE) {
    errno = EAFNOSUPPORT;
    return -1;
  }
  if (!next) next = (int (*)(int, int, int))dlsym(RTLD_NEXT, "socket");
  return next(domain, type, protocol);
}
