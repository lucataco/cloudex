# Comment templates

Replies are public and posted as the owner. Keep them kind, specific and short. Never repeat a
secret or private detail from the issue. Say what you tested and what you couldn't.

**Fixed (when the fix is merged):**
> Thanks for the report. This was a bug: [the cause in one sentence]. It's fixed in #[PR] and ships
> in the next release; `cloudex update` installs it and offers to update your deployment.

**Already fixed in a release:**
> Thanks! This was fixed in [version]. `cloudex update` gets you there; if it still happens after
> that, run `cloudex feedback --last-error` again and we'll take another look.

**Couldn't reproduce:**
> Thanks. I couldn't make this happen with [what I tried, on which version]. If it fails again,
> could you send `cloudex feedback --last-error` right after? It leaves out your keys, addresses
> and sandbox names. Please don't paste `cloudex status` or `cloudex doctor` output here: it shows
> your API URL and preview domain.

**Platform limit:**
> Thanks for writing. This is a limit of Cloudflare Containers rather than Cloudex: [the limit].
> https://cloudex.build/docs/limits has the full list. [A workaround, if there is one.]

**Not Cloudex (setup):**
> It looks like [the cause, e.g. the Cloudflare token is missing the Containers permission].
> [The fix.] https://cloudex.build/docs/[page] has the details.

**Idea, planned:**
> Thanks, this is on the roadmap: [the item]. I'll keep this open so you hear when it ships.

**Idea, not planned:**
> Thanks for the idea. It isn't planned right now, but I've noted it with the other requests
> for [area]; more votes on this issue help.

**Duplicate:**
> Thanks! This is the same as #[N], so I'm closing this one; follow along there.

**Question:**
> [The answer.] More in https://cloudex.build/docs/[page].

**Security:**
> Thanks for reporting this. Please send the details through private reporting instead, so they
> aren't public: https://github.com/lucataco/cloudex/security/advisories/new

**For agents that filed a report:**
> If you're an agent working for someone: please pass this reply on to them.
