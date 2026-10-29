#!/usr/bin/env python3
"""Runs shell commands on a booting OpenWrt VM through its serial console (VM flash test).

usage: console.py PORT COMMAND_FILE   (QEMU listens on PORT as a TCP serial server)
Prints the console output; exits 1 if the shell never answers.
"""
import socket
import sys
import time

port, command_file = int(sys.argv[1]), sys.argv[2]
commands = open(command_file).read().strip()
deadline = time.time() + 420


def connect():
    while time.time() < deadline:
        try:
            return socket.create_connection(("127.0.0.1", port), timeout=5)
        except OSError:
            time.sleep(1)
    sys.exit("console: QEMU serial port never opened")


sock = connect()
sock.settimeout(2)
seen = b""


def read():
    global seen
    try:
        data = sock.recv(65536)
    except socket.timeout:
        return b""
    if not data:
        sys.exit("console: QEMU closed the serial port")
    seen += data
    sys.stdout.buffer.write(data)
    sys.stdout.flush()
    return data


# The console shell starts after "Please press Enter to activate this console."
while time.time() < deadline:
    sock.sendall(b"\r\n")
    read()
    sock.sendall(b"echo SHELL-$((6*7))\r\n")
    time.sleep(1)
    read()
    if b"SHELL-42" in seen:
        break
else:
    sys.exit("console: the VM shell never answered")

seen = b""
sock.sendall((commands + "\necho DONE-$((6*7))\r\n").encode())
while time.time() < deadline:
    read()
    if b"DONE-42" in seen:
        print("\nconsole: commands finished")
        sys.exit(0)
sys.exit("console: commands did not finish")
