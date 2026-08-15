// SPDX-License-Identifier: GPL-2.0
/*
 * gluon_sim — a stand-in for the NE's `gluon` hardware-simulation module.
 *
 * It exposes one thing over generic netlink: a device ID, the way real silicon
 * would expose a burned-in serial. That is enough to prove the transport the
 * real stack depends on (Atom userspace <-> kwark/gluon in-kernel over
 * gennetlink) works inside a Plasma VM.
 *
 * The ID is set at insmod time so each VM in a fleet gets a distinct identity:
 *     insmod gluon_sim.ko device_id=0x0a000002
 */

#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/init.h>
#include <net/genetlink.h>

#define GLUON_FAMILY_NAME "gluon_sim"
#define GLUON_VERSION     1

/* Default so a bare `insmod gluon_sim.ko` still works; a fleet overrides it. */
#define GLUON_DEFAULT_DEVICE_ID 0xDEADBEEFULL

static unsigned long long device_id = GLUON_DEFAULT_DEVICE_ID;
module_param(device_id, ullong, 0444);
MODULE_PARM_DESC(device_id, "Static per-VM device identifier reported to userspace");

/* Attributes */
enum {
	GLUON_A_UNSPEC,
	GLUON_A_DEVICE_ID,	/* u64 */
	__GLUON_A_MAX,
};
#define GLUON_A_MAX (__GLUON_A_MAX - 1)

/* Commands */
enum {
	GLUON_CMD_UNSPEC,
	GLUON_CMD_GET_DEVICE_ID,
	__GLUON_CMD_MAX,
};
#define GLUON_CMD_MAX (__GLUON_CMD_MAX - 1)

static const struct nla_policy gluon_policy[GLUON_A_MAX + 1] = {
	[GLUON_A_DEVICE_ID] = { .type = NLA_U64 },
};

static struct genl_family gluon_family;

/*
 * Reply to a GET_DEVICE_ID with the configured ID. Unicast back to the
 * caller's portid so several userspace clients can query independently.
 */
static int gluon_get_device_id(struct sk_buff *skb, struct genl_info *info)
{
	struct sk_buff *msg;
	void *hdr;
	int ret;

	msg = genlmsg_new(nla_total_size(sizeof(u64)) + GENL_HDRLEN, GFP_KERNEL);
	if (!msg)
		return -ENOMEM;

	hdr = genlmsg_put(msg, info->snd_portid, info->snd_seq, &gluon_family, 0,
			  GLUON_CMD_GET_DEVICE_ID);
	if (!hdr) {
		ret = -EMSGSIZE;
		goto err;
	}

	/* NLA_U64 needs explicit padding to stay 8-byte aligned. */
	ret = nla_put_u64_64bit(msg, GLUON_A_DEVICE_ID, (u64)device_id, NLA_F_NESTED);
	if (ret)
		goto err;

	genlmsg_end(msg, hdr);
	return genlmsg_reply(msg, info);

err:
	nlmsg_free(msg);
	return ret;
}

static const struct genl_small_ops gluon_ops[] = {
	{
		.cmd    = GLUON_CMD_GET_DEVICE_ID,
		.doit   = gluon_get_device_id,
		/* No privilege requirement: this is a read-only identity probe. */
		.flags  = 0,
	},
};

static struct genl_family gluon_family = {
	.name       = GLUON_FAMILY_NAME,
	.version    = GLUON_VERSION,
	.maxattr    = GLUON_A_MAX,
	.policy     = gluon_policy,
	.netnsok    = false,
	.module     = THIS_MODULE,
	.small_ops  = gluon_ops,
	.n_small_ops = ARRAY_SIZE(gluon_ops),
	.resv_start_op = GLUON_CMD_GET_DEVICE_ID + 1,
};

static int __init gluon_sim_init(void)
{
	int ret = genl_register_family(&gluon_family);

	if (ret) {
		pr_err("gluon_sim: genl_register_family failed: %d\n", ret);
		return ret;
	}

	/* Printed so `dmesg | grep gluon_sim` alone proves the module loaded. */
	pr_info("gluon_sim: loaded, device_id=0x%llx, genl family '%s'\n",
		device_id, GLUON_FAMILY_NAME);
	return 0;
}

static void __exit gluon_sim_exit(void)
{
	genl_unregister_family(&gluon_family);
	pr_info("gluon_sim: unloaded\n");
}

module_init(gluon_sim_init);
module_exit(gluon_sim_exit);

MODULE_LICENSE("GPL");
MODULE_AUTHOR("Fortaegis");
MODULE_DESCRIPTION("Simulated NE hardware layer exposing a device ID over generic netlink");
MODULE_VERSION("0.1");
