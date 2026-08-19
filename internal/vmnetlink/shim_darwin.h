// vmnet.framework is a block-and-dispatch-queue API, which cgo cannot express.
// This shim wraps it in plain C functions with blocking semantics that map
// cleanly onto Go calls. Keep this header free of framework includes so cgo
// only has to parse stdint/stddef.

#ifndef PT_VMNETLINK_SHIM_H
#define PT_VMNETLINK_SHIM_H

#include <stddef.h>
#include <stdint.h>

typedef struct vmnetlink vmnetlink_t;

// Return codes. Non-negative values are success (for read, the frame length in
// bytes); negative values are failures. VMNETLINK_ERR_VMNET means the vmnet
// status out-parameter holds the framework's own vmnet_return_t.
#define VMNETLINK_OK 0
#define VMNETLINK_ERR_VMNET (-1)
#define VMNETLINK_ERR_CLOSED (-2)
#define VMNETLINK_ERR_NOMEM (-3)
#define VMNETLINK_ERR_START (-4)
#define VMNETLINK_ERR_NOTWRITTEN (-5)

// vmnetlink_open starts a shared-mode interface on the given subnet and
// registers the packets-available event callback. Requires euid 0.
int vmnetlink_open(const char *start_address, const char *end_address,
                   const char *subnet_mask, int isolation, vmnetlink_t **out,
                   uint32_t *status);

// vmnetlink_read blocks until a frame is available and leaves it in the link's
// receive buffer, returning its length. The buffer is valid until the next
// vmnetlink_read on the same link, so at most one reader may be in flight.
int vmnetlink_read(vmnetlink_t *l, uint32_t *status);

// vmnetlink_write writes exactly one frame. It does not block on the reader and
// may run concurrently with vmnetlink_read.
int vmnetlink_write(vmnetlink_t *l, const void *frame, size_t len,
                    uint32_t *status);

// vmnetlink_close stops the interface. It waits for in-flight read/write calls
// to return (waking a blocked reader with VMNETLINK_ERR_CLOSED first) so the
// interface is never stopped out from under vmnet_read. Idempotent.
int vmnetlink_close(vmnetlink_t *l, uint32_t *status);

// vmnetlink_free closes if needed and releases the link. It must only be called
// once no other thread can reach l.
void vmnetlink_free(vmnetlink_t *l);

const void *vmnetlink_rxbuf(const vmnetlink_t *l);
uint64_t vmnetlink_max_packet_size(const vmnetlink_t *l);
const char *vmnetlink_gateway(const vmnetlink_t *l);

#endif /* PT_VMNETLINK_SHIM_H */
