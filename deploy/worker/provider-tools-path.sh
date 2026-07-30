# Keep Provider CLI and package-manager shims visible when coding agents start
# login shells. Alpine and Debian /etc/profile reset PATH before sourcing this
# file, so relying on the image ENV alone is insufficient.
export PATH=/opt/synara/provider-tools/node_modules/.bin:/home/synara/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
