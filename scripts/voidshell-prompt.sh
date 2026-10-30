# Reflect the SSH username injected by voidshell in the shell prompt.
# USER and LOGNAME match the Linux account (voidshell) for tool compatibility.
# VOIDSHELL_USER is the display name and does not need a passwd entry.
if [ -n "$VOIDSHELL_USER" ]; then
    PS1="${VOIDSHELL_USER}@\h:\w\$ "
    export PS1
fi
