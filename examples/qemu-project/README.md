# QEMU example project

This tiny project is used by the QEMU end-to-end test. The test initializes a
Git repository, starts the predictable dummy agent, interacts with it through
its container TTY, and verifies that its changes are accepted or discarded.
