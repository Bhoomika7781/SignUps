# AI prompt log

Every prompt we sent to an AI tool during the hackathon, in chronological
order, unedited. Times are IST (UTC+05:30) and approximate.

<!--
TEAM TODO (delete this comment when done):
1. Paste every prompt from the ChatGPT conversation(s) below, word for word,
   in order, with approximate times. Do not clean them up: judges reward an
   honest log and punish a missing one (-10 points).
2. Add every prompt sent to any other tool (Copilot, Gemini, Claude, ...).
3. Keep adding prompts until the deadline.
-->

## ChatGPT (from about 11:30 to 19:00)

> TODO: paste the ChatGPT prompts here, one block per prompt, with time.

## Claude (claude.ai)

**~19:21**
> I am migrating a conversation over from ChatGPT. I have uploaded the original file we were working on, as well as the markdown file containing our entire chat history. Please thoroughly review the chat log to understand the current state of the project, the specific feedback or edits I requested, and my stylistic preferences. Once you have read both, please summarize the last action item we were working on and let me know you are ready to continue.

**~23:08** (attached: zip of the repository and the problem statement PDF)
> this is the progress of the project zip file and the ps pdf ive attached. analyse the pdf and guide me to complete the project obeying the rules and say me if the ui is needed or not.

What came back: a review of our engine. It found that batches were not atomic
(one fsync per record), that recovery failed on a torn tail, and that the
`go 1.27.3` go.mod would break an offline build. It then rewrote the engine to
use one frame per write and per batch, and added `kvdb` (all commands plus
serve), the torture tool, unit tests and drafts of the docs. It also ran a
mutation test: with batches made non-atomic again, the torture tool caught the
bug in iteration 1.

TODO (team): write down what WE did to verify it. For example: which crash
points we checked against the code, `go test ./tests/` and torture results on
our laptop, and anything we changed or pushed back on.

<!-- Add your follow-up prompts below, e.g. questions you asked to understand a crash point. -->
