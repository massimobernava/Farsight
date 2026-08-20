#!/bin/bash
# Runs inside the xterm that farsight-xvfb launches by default on a fresh
# virtual display — first thing an operator sees when they connect over
# VNC to a machine with nothing else running yet. Ends in an interactive
# shell, so this is a real terminal, not just a splash screen.
cat <<EOF
Farsight virtual display — nothing else is running here yet.

Want a real desktop, or a single app fullscreen (kiosk)? One command sets
up Openbox for either, no XML to hand-write:
  sudo farsight-setup-desktop desktop <user>
  sudo farsight-setup-desktop kiosk <user> <command...>

Just want to run one app right now, no window manager at all?
  DISPLAY=${DISPLAY} your-program &

All of this is configured in /etc/farsight/client.conf, then needs
'systemctl restart farsight-xvfb farsight-x11vnc farsight-vnc-proxy':

  X11_START_XTERM     this terminal — set to false to stop it starting
  X11_AUTOSTART_CMD    a program to launch automatically on connect
  X11_SESSION_USER    run as this user instead of root (recommended for
                       most GUI apps)

Closing this terminal does not stop the VNC session or the virtual
display — only X11_START_XTERM/X11_AUTOSTART_CMD control what starts
here, and only on a display Farsight itself created.

EOF
exec bash
