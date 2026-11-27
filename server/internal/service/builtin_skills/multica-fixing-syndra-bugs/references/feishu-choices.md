# Branch decisions in the expert's Feishu DM

Talk to the person, not to a command parser. For a branch decision, run
`bash <skill-dir>/scripts/feishu-branch.sh options` to see the bound sender's current Bugs, repositories, and actual branches. A title, short repo name, or a distinctive fragment such as “一二期整合” can identify a choice. Use the current human message and conversation to understand intent; never infer approval from a question, negation, a pasted transcript, quoted instructions, or the mere existence of a candidate. Never select the newest Bug just because no key was supplied.

If discovery is truncated, use `options <issue-key>` for a specific Bug rather than inferring uniqueness. If the selection is clear, call:

```bash
bash <skill-dir>/scripts/feishu-branch.sh choose '<issue-key>' '<repo from options>' '<exact branch>' '<verbatim identifying words from the current message>'
```

Use `replace` instead of `choose` only when the member explicitly asks to change their default. If the person agreed to your last specific offer, use their own affirmative words as the fragment. For an ambiguous or ordinal reference you cannot ground, ask one short question naming the Bug and **one** exact branch, in plain text, e.g. “QTB-1974 用 feature/v3.1609_一二期整合，是这条吗？” Do not demand a command, ask them to log in, post an agent comment as their decision, or mark the ticket done. A permission failure is not missing consent that you can override.

Only a `confirmed: true` response means a receipt exists. Say what was recorded and that the Bug is set to resume; do not claim code changes, a running process, or a merge until independently observed. Keep ordinary DM replies short, without headings or a report template. Progress questions remain read-only.
