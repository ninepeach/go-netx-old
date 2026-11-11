package socket_test

import "syscall"

// errPermission is EPERM, which SO_MARK returns without CAP_NET_ADMIN.
var errPermission error = syscall.EPERM
