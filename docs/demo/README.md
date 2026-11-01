# Recording the demo

[`demo.sh`](demo.sh) is a 20-second terminal demo of the queue: four agents' gates each ask for 60 %
of the machine's memory, so one runs and three wait in order, and `coreslot status` shows the
running command and the queue at each step. It runs on a scratch state directory
(`CORESLOT_STATE_DIR` in a temporary directory), so it never touches the machine's real one. The
gates are `sleep`s: the demo shows the mechanics only.

With [asciinema](https://asciinema.org) and its GIF renderer
[agg](https://github.com/asciinema/agg):

```sh
asciinema rec --cols 100 --rows 30 -c docs/demo/demo.sh docs/demo/demo.cast
agg --font-size 16 docs/demo/demo.cast docs/demo/demo.gif
```

Commit `demo.gif` and put it under the README's first code block. A `.cast` file can also be
uploaded to asciinema.org and linked instead of a GIF.
