savedcmd_gluon_sim.mod := printf '%s\n'   gluon_sim.o | awk '!x[$$0]++ { print("./"$$0) }' > gluon_sim.mod
