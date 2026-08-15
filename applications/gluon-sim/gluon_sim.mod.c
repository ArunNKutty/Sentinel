#include <linux/module.h>
#include <linux/export-internal.h>
#include <linux/compiler.h>

MODULE_INFO(name, KBUILD_MODNAME);

__visible struct module __this_module
__section(".gnu.linkonce.this_module") = {
	.name = KBUILD_MODNAME,
	.init = init_module,
#ifdef CONFIG_MODULE_UNLOAD
	.exit = cleanup_module,
#endif
	.arch = MODULE_ARCH_INIT,
};



static const struct modversion_info ____versions[]
__used __section("__versions") = {
	{ 0x5b8239ca, "__x86_return_thunk" },
	{ 0x82bf0309, "genl_unregister_family" },
	{ 0x3bb72995, "__alloc_skb" },
	{ 0xec1a3fb0, "genlmsg_put" },
	{ 0x838fede8, "nla_put_64bit" },
	{ 0xbadc89bb, "sk_skb_reason_drop" },
	{ 0xe8f4b41f, "netlink_unicast" },
	{ 0xf0fdf6cb, "__stack_chk_fail" },
	{ 0x3bf035f6, "param_ops_ullong" },
	{ 0xbdfb6dbb, "__fentry__" },
	{ 0x3eccc468, "genl_register_family" },
	{ 0x92997ed8, "_printk" },
	{ 0xbf1981cb, "module_layout" },
};

MODULE_INFO(depends, "");


MODULE_INFO(srcversion, "12490141E111623C359B305");
