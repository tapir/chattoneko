/* musl's libc.a is built with unwind tables; nothing in a C-only static binary unwinds */
SECTIONS { /DISCARD/ : { *(.eh_frame) } }
INSERT AFTER .text;
