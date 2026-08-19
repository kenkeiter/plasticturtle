#include "shim_darwin.h"

#include <Block.h>
#include <dispatch/dispatch.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <sys/uio.h>
#include <vmnet/vmnet.h>

struct vmnetlink {
	interface_ref iface;
	dispatch_queue_t ctlq; // start/stop completion handlers
	dispatch_queue_t evq;  // packets-available callback
	vmnet_interface_event_callback_t evcb;

	pthread_mutex_t mu;
	pthread_cond_t wake; // a packet event arrived, or the link was closed
	pthread_cond_t idle; // the last in-flight read/write returned
	int ready;
	int closed;
	int stopped;
	int busy;

	uint64_t max_packet_size;
	char gateway[64];
	uint8_t *rxbuf;
};

// enter/leave bound the window in which a thread may touch the vmnet interface.
// vmnet_stop_interface() while another thread sits in vmnet_read() is a
// use-after-free, so close() waits for busy to drain instead of racing it.
static int enter(vmnetlink_t *l) {
	pthread_mutex_lock(&l->mu);
	if (l->closed) {
		pthread_mutex_unlock(&l->mu);
		return -1;
	}
	l->busy++;
	pthread_mutex_unlock(&l->mu);
	return 0;
}

static void leave(vmnetlink_t *l) {
	pthread_mutex_lock(&l->mu);
	if (--l->busy == 0) {
		pthread_cond_broadcast(&l->idle);
	}
	pthread_mutex_unlock(&l->mu);
}

static void destroy(vmnetlink_t *l) {
	if (l->evcb != NULL) {
		Block_release(l->evcb);
	}
	if (l->ctlq != NULL) {
		dispatch_release(l->ctlq);
	}
	if (l->evq != NULL) {
		dispatch_release(l->evq);
	}
	free(l->rxbuf);
	pthread_cond_destroy(&l->idle);
	pthread_cond_destroy(&l->wake);
	pthread_mutex_destroy(&l->mu);
	free(l);
}

int vmnetlink_open(const char *start_address, const char *end_address,
                   const char *subnet_mask, int isolation, vmnetlink_t **out,
                   uint32_t *status) {
	*out = NULL;
	*status = 0;

	vmnetlink_t *l = calloc(1, sizeof(*l));
	if (l == NULL) {
		return VMNETLINK_ERR_NOMEM;
	}
	pthread_mutex_init(&l->mu, NULL);
	pthread_cond_init(&l->wake, NULL);
	pthread_cond_init(&l->idle, NULL);

	// Private serial queues: the completion handlers and the event callback
	// must never contend for the main queue, which a Go process does not run.
	l->ctlq = dispatch_queue_create("com.plasticturtle.vmnetlink.ctl", DISPATCH_QUEUE_SERIAL);
	l->evq = dispatch_queue_create("com.plasticturtle.vmnetlink.events", DISPATCH_QUEUE_SERIAL);
	if (l->ctlq == NULL || l->evq == NULL) {
		destroy(l);
		return VMNETLINK_ERR_NOMEM;
	}

	xpc_object_t desc = xpc_dictionary_create(NULL, NULL, 0);
	xpc_dictionary_set_uint64(desc, vmnet_operation_mode_key, VMNET_SHARED_MODE);
	xpc_dictionary_set_string(desc, vmnet_start_address_key, start_address);
	xpc_dictionary_set_string(desc, vmnet_end_address_key, end_address);
	xpc_dictionary_set_string(desc, vmnet_subnet_mask_key, subnet_mask);
	xpc_dictionary_set_bool(desc, vmnet_enable_isolation_key, isolation != 0);

	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	__block vmnet_return_t start_status = VMNET_FAILURE;

	l->iface = vmnet_start_interface(desc, l->ctlq, ^(vmnet_return_t st, xpc_object_t params) {
		start_status = st;
		if (st == VMNET_SUCCESS && params != NULL) {
			l->max_packet_size = xpc_dictionary_get_uint64(params, vmnet_max_packet_size_key);
			const char *gw = xpc_dictionary_get_string(params, vmnet_start_address_key);
			if (gw != NULL) {
				strlcpy(l->gateway, gw, sizeof(l->gateway));
			}
		}
		dispatch_semaphore_signal(done);
	});
	xpc_release(desc);

	if (l->iface == NULL) {
		dispatch_release(done);
		destroy(l);
		return VMNETLINK_ERR_START;
	}
	dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
	dispatch_release(done);

	if (start_status != VMNET_SUCCESS) {
		*status = start_status;
		destroy(l);
		return VMNETLINK_ERR_VMNET;
	}
	if (l->max_packet_size == 0) {
		destroy(l);
		return VMNETLINK_ERR_START;
	}

	l->rxbuf = malloc((size_t)l->max_packet_size);
	if (l->rxbuf == NULL) {
		destroy(l);
		return VMNETLINK_ERR_NOMEM;
	}

	// The callback is only a wakeup: it flags readiness and returns, leaving the
	// reader to drain until vmnet_read() reports no packets. Holding the
	// callback (as softnet does) would keep a vmnet dispatch thread parked.
	l->evcb = Block_copy(^(interface_event_t mask, xpc_object_t event) {
		(void)mask;
		(void)event;
		pthread_mutex_lock(&l->mu);
		l->ready = 1;
		pthread_cond_signal(&l->wake);
		pthread_mutex_unlock(&l->mu);
	});
	vmnet_return_t cb_status = vmnet_interface_set_event_callback(
	    l->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, l->evq, l->evcb);
	if (cb_status != VMNET_SUCCESS) {
		*status = cb_status;
		uint32_t ignored = 0;
		vmnetlink_close(l, &ignored);
		destroy(l);
		return VMNETLINK_ERR_VMNET;
	}

	*out = l;
	return VMNETLINK_OK;
}

int vmnetlink_read(vmnetlink_t *l, uint32_t *status) {
	*status = 0;
	if (enter(l) != 0) {
		return VMNETLINK_ERR_CLOSED;
	}

	int ret;
	for (;;) {
		struct iovec iov = {
		    .iov_base = l->rxbuf,
		    .iov_len = (size_t)l->max_packet_size,
		};
		struct vmpktdesc pkt = {
		    .vm_pkt_size = iov.iov_len,
		    .vm_pkt_iov = &iov,
		    .vm_pkt_iovcnt = 1,
		    .vm_flags = 0,
		};
		int pktcnt = 1;

		vmnet_return_t st = vmnet_read(l->iface, &pkt, &pktcnt);
		if (st != VMNET_SUCCESS) {
			*status = st;
			ret = VMNETLINK_ERR_VMNET;
			break;
		}
		if (pktcnt == 1) {
			ret = (int)pkt.vm_pkt_size;
			break;
		}

		// Nothing queued. Clear the flag before looping back to read so a packet
		// that lands during the read is not mistaken for one we already drained.
		pthread_mutex_lock(&l->mu);
		while (!l->ready && !l->closed) {
			pthread_cond_wait(&l->wake, &l->mu);
		}
		int closed = l->closed;
		l->ready = 0;
		pthread_mutex_unlock(&l->mu);
		if (closed) {
			ret = VMNETLINK_ERR_CLOSED;
			break;
		}
	}

	leave(l);
	return ret;
}

int vmnetlink_write(vmnetlink_t *l, const void *frame, size_t len, uint32_t *status) {
	*status = 0;
	if (enter(l) != 0) {
		return VMNETLINK_ERR_CLOSED;
	}

	struct iovec iov = {
	    .iov_base = (void *)frame,
	    .iov_len = len,
	};
	struct vmpktdesc pkt = {
	    .vm_pkt_size = len,
	    .vm_pkt_iov = &iov,
	    .vm_pkt_iovcnt = 1,
	    .vm_flags = 0,
	};
	int pktcnt = 1;

	vmnet_return_t st = vmnet_write(l->iface, &pkt, &pktcnt);
	leave(l);

	if (st != VMNET_SUCCESS) {
		*status = st;
		return VMNETLINK_ERR_VMNET;
	}
	if (pktcnt != 1) {
		return VMNETLINK_ERR_NOTWRITTEN;
	}
	return VMNETLINK_OK;
}

int vmnetlink_close(vmnetlink_t *l, uint32_t *status) {
	*status = 0;

	pthread_mutex_lock(&l->mu);
	if (l->stopped) {
		pthread_mutex_unlock(&l->mu);
		return VMNETLINK_OK;
	}
	l->closed = 1;
	pthread_cond_broadcast(&l->wake);
	while (l->busy > 0) {
		pthread_cond_wait(&l->idle, &l->mu);
	}
	l->stopped = 1;
	pthread_mutex_unlock(&l->mu);

	if (l->evcb != NULL) {
		vmnet_interface_set_event_callback(l->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, NULL, NULL);
		// Barrier on the serial event queue: once this returns, no callback is
		// still running, so the block's captured state can be torn down.
		dispatch_sync(l->evq, ^{
		});
	}

	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	__block vmnet_return_t stop_status = VMNET_FAILURE;
	vmnet_return_t st = vmnet_stop_interface(l->iface, l->ctlq, ^(vmnet_return_t s) {
		stop_status = s;
		dispatch_semaphore_signal(done);
	});
	if (st != VMNET_SUCCESS) {
		dispatch_release(done);
		*status = st;
		return VMNETLINK_ERR_VMNET;
	}
	dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
	dispatch_release(done);

	if (stop_status != VMNET_SUCCESS) {
		*status = stop_status;
		return VMNETLINK_ERR_VMNET;
	}
	return VMNETLINK_OK;
}

void vmnetlink_free(vmnetlink_t *l) {
	if (l == NULL) {
		return;
	}
	uint32_t ignored = 0;
	vmnetlink_close(l, &ignored);
	destroy(l);
}

const void *vmnetlink_rxbuf(const vmnetlink_t *l) { return l->rxbuf; }

uint64_t vmnetlink_max_packet_size(const vmnetlink_t *l) { return l->max_packet_size; }

const char *vmnetlink_gateway(const vmnetlink_t *l) { return l->gateway; }
