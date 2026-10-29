# Recording the demo

[`demo.sh`](demo.sh) is a 20-second terminal demo of the queue: four agents' gates ask for 4
cores each on a budget of 8, two run, two wait, and `coreslot status` shows the holds and the
queue at each step. It runs on a scratch budget (`CORESLOT_STATE_DIR` in a temporary directory),
so it never touches the machine's real one. The gates are `sleep`s: the demo shows the
mechanics, and [the evidence](../../evidence/README.md) has the measured effect on real suites.

With [asciinema](https://asciinema.org) and its GIF renderer
[agg](https://github.com/asciinema/agg):

```sh
asciinema rec --headless --window-size 120x32 --overwrite -c docs/demo/demo.sh docs/demo/demo.cast
agg --font-size 16 docs/demo/demo.cast docs/demo/demo.gif
```

The committed [`demo.cast`](demo.cast) and [`demo.gif`](demo.gif) were made this way, with
asciinema 3.2.1 and agg 1.9.0; the README shows the GIF under its first code block. A `.cast`
file can also be uploaded to asciinema.org and linked instead of a GIF.
