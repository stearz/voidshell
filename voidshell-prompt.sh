# Reflect the SSH username injected by voidshell in the shell prompt.
# USER and LOGNAME are also set by the pod spec so tools that read them show
# the right name, but whoami (which looks up /etc/passwd by UID) returns "voidshell".
if [ -n "$VOIDSHELL_USER" ]; then
    PS1="${VOIDSHELL_USER}@\h:\w\$ "
    export PS1
fi
