<script>
  // Crush-style waiting cursor: 15 glyphs that start as dots, then three
  // random positions flip every 20ms, never re-picking one changed on the
  // previous tick. Mount it while something is in flight (a message being
  // sent, a reply being generated); unmounting stops the interval.
  let text = $state('');
  $effect(() => {
    const glyph = () => String.fromCharCode(33 + Math.floor(Math.random() * 94));
    const chars = Array.from({ length: 15 }, () => '.');
    text = chars.join('');
    let last = new Set();
    const t = setInterval(() => {
      const picked = new Set();
      while (picked.size < 3) {
        const i = Math.floor(Math.random() * 15);
        if (!last.has(i)) picked.add(i);
      }
      last = picked;
      for (const i of picked) chars[i] = glyph();
      text = chars.join('');
    }, 20);
    return () => clearInterval(t);
  });
</script>

<span class="stream-scramble">{text}</span>
