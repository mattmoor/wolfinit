# chainit

An experimental trivial init system for running a container entrypoint and cmd
without the need for a shell.  This implements the intended semantics of apko's
configured entrypoint when invoked by reading `/etc/apko.json`.
