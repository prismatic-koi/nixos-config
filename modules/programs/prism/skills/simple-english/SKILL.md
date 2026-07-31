---
name: simple-english
description: |
  Write or rewrite text with the rules of ASD-STE100 Simplified Technical
  English so it is clear and free of AI slop. Use for documentation, READMEs,
  runbooks, procedures, error messages, release notes, incident reports, PR
  descriptions, commit bodies, and API guides. Also use when the user says
  "STE", "Simplified Technical English", "ASD-STE100", "de-slop", "make this
  readable", or asks for text that reads well for a non-native reader. Also
  use when writing decision-support text: a finding, an option set, a risk
  report, or an escalation. Applies the rule set of the standard:
  sentence-length limits, one word for one meaning, simple tenses, active
  voice, and condition before command.
license: MIT
compatibility: claude-code cursor codex gemini-cli opencode
metadata:
  standard: ASD-STE100 (adapted)
  upstream: AminBlg/SimpleEnglish
---

# Simple English: write for the reader who cannot ask a question

Write with the rules of ASD-STE100 Simplified Technical English. STE is the
controlled language that aerospace and defence manufacturers use for
maintenance documentation. The rules exist so that a tired reader who is not a native English
speaker cannot misread an instruction. As a side
effect, the rules remove the usual signs of AI-generated text: long
sentences, synonym rotation, hedges, filler, and decorative clauses.

Write for that tired reader. Each sentence must survive one read.

This skill adapts the rule catalogue from
[AminBlg/SimpleEnglish](https://github.com/AminBlg/SimpleEnglish) (MIT
licence) for this repository. See "Attribution and disclaimer" at the end of
this file.

## Your task

When you write or rewrite technical text:

1. Select the mode: pragmatic or strict (below).
2. Select the register class: A, B, or C (below). The class sets the strictness
   of the rules that follow.
3. Classify each passage as procedural or descriptive. Every other rule
   depends on this step.
4. Fix your vocabulary before you draft. Pick one verb for the
   check/verify/confirm/validate concept and one noun for the
   config/settings concept. Use no other word for these concepts in the whole
   document.
5. Apply the rules from the rule catalogue.
6. Run the self-check before you deliver the text. This step is mandatory.
7. Never change code, identifiers, commands, or quoted output. See
   "Untouchables".

When a user asks you to check text instead of writing it, report each
violation as: rule number, the offending text, and a compliant rewrite. Cite
only rule numbers that appear in this file.

## Two modes

| Mode | When | What you apply |
|---|---|---|
| Pragmatic (default) | Docs, READMEs, error messages, and most agent output | All structural rules. Domain words stay: "idempotent", "webhook". |
| Strict | The user names STE, ASD-STE100, or compliance | Structural rules plus full vocabulary discipline. Tell the user that full compliance needs the official dictionary, a free download at asd-ste100.org. |

## The three-class register model

STE governs written artifacts. Agent output also includes live conversation,
so this skill adds a register model on top of the rule catalogue. The
governing axis is decision-relevant load, not "chat versus artifact".

| Class | Applies to | Rules |
|---|---|---|
| A — Artifact | Docs, PR descriptions, commit bodies, error messages, incident reports, acceptance criteria, runbooks, change requests, issue bodies, agent instructions | Full STE. No Te Reo except where it names a thing (a service, a host, a repo, a product). NZ spelling. |
| B — Decision-support | The agent explains a finding, presents options, reports a risk, asks the user to decide, escalates, or summarises a review outcome | Full STE structural rules. Te Reo in framing position only (see below). |
| C — Conversational | Acknowledgement, rapport, social framing | Casual register. Te Reo is free within existing project guidance. The slop ban and the hedge ban still apply. The sentence-length limits and the modal ban relax. |

Test for class B: if the user can make a wrong decision because they
misread the sentence, the sentence is class B.

Default to class B when the class is not clear. Class C is the narrow
exception, not the fallback.

Mixing rule: when one response spans two or more classes, the strictest
applicable class governs the substantive content.

## The Te Reo framing-position rule

Te Reo can sit in framing position: a greeting, a sign-off, an
acknowledgement, a transition, or any clause that states no fact the user
must act on. Te Reo must not sit inside the sentence that states the
finding, the risk, the option, or the recommendation. Use English in those
sentences.

Cross-check with the deletion test: delete the Te Reo term. If the decision
content is unchanged, the placement was framing, and it is correct. If the
reader loses the decision content, or must guess at it, rewrite that
sentence in English.

The positional rule governs. The deletion test is a cross-check, not a
replacement for it. A pure deletion test has a gap: "My whakaaro is that we
drop the check" survives deletion of "whakaaro" with the meaning intact, but
the term still sits inside the payload sentence, so the positional rule
still fails it.

Worked examples:

- PASS: "Ka pai — here is the tradeoff. Option A costs one extra CI job."
- PASS: "Good kōrero. Three options follow."
- PASS: "That is the mahi for step 1. Ngā mihi."
- FAIL: "The mahi depends on whether the lint lands first." Corrected: "The
  work depends on whether the lint lands first."
- FAIL: "My whakaaro is that we drop the modal check." Corrected: "I
  recommend that we drop the modal check."

## Step 1: classify the text

| | Procedural (instructions) | Descriptive (explanations) |
|---|---|---|
| Purpose | Tell the reader what to do | Explain what a thing is or does |
| Verb form | Imperative: "Install the pump." | Simple present, simple past, or simple future |
| Sentence limit | 20 words (Rule 5.1) | 25 words (Rule 6.3) |
| Unit rule | One instruction per sentence (5.2) | One topic per paragraph (6.5), maximum six sentences per paragraph (6.6) |

Do not mix the two forms in one passage. A "Getting started" section is
procedural. An "Architecture" section is descriptive. A note inside a
procedure is descriptive: it takes the 25-word limit and no imperative.

## The rule catalogue

Nine sections, paraphrased from ASD-STE100 with software examples. The
official wording is in the free standard at asd-ste100.org.

### Section 1 — Words (Rules 1.1-1.14)

| Rule | Instruction |
|---|---|
| 1.1 | Use only approved words, technical nouns, or technical verbs. |
| 1.2 | Use an approved word only as its listed part of speech. |
| 1.3 | Use an approved word only with its approved meaning. |
| 1.4 | Use only the approved forms of verbs and adjectives. |
| 1.5 | You can use domain words as technical nouns: "webhook", "commit", "endpoint". |
| 1.6 | Use an unapproved word only when it is a technical noun or part of one. |
| 1.7 | Do not use technical nouns as verbs. |
| 1.8 | Use the technical nouns of your project or industry. |
| 1.9 | When you pick a technical noun, pick a short and clear one. |
| 1.10 | Do not use regional, slang, or jargon words as technical nouns. |
| 1.11 | One item takes one name. Do not call it "config" in one place and "settings" in another. |
| 1.12 | You can use domain verbs as technical verbs: "deploy", "compile", "merge". |
| 1.13 | Do not use technical verbs as nouns. |
| 1.14 | Use New Zealand English spelling: `-ise`/`-isation` not `-ize`/`-ization`, `-yse` not `-yze`, `behaviour`, `defence`, `colour`, `organisation`, `centre`. Noun `licence`, verb `license`. |

Rule 1.14 differs from the upstream ASD-STE100 catalogue, which mandates
American spelling. The intent behind the original rule stands: pick one
spelling convention and apply it consistently through the whole document.
Only the dialect changes, to match the convention of this repository.

Carve-out for Rule 1.14: identifiers, config keys, API field names, and
quoted output keep their real spelling, even when that spelling is US
English. The `Authorization` header, a `color` CSS property, an
`initialize()` method, and a `behavior` field in a third-party API are
untouchable. Do not "correct" the spelling of a working identifier.

In pragmatic mode, rules 1.5, 1.8, and 1.12 do the heavy lifting: your
domain vocabulary is legal. The rules that agents most often break are 1.7,
1.11, and 1.13.

Before: "You can webhook the event, then do a deploy."
After: "Send the event to the webhook. Then deploy the service."

### Section 2 — Multi-word nouns (Rules 2.1-2.2)

| Rule | Instruction |
|---|---|
| 2.1 | Write multi-word nouns of three words or fewer. |
| 2.2 | When a technical noun needs more than three words, write it in full once, then give a short form, or hyphenate the units. |

Break long noun chains with prepositions: "of", "on", "in", "for".

Before: "the connection pool timeout configuration value"
After: "the timeout value for the connection pool"

### Section 3 — Verbs (Rules 3.1-3.7)

| Rule | Instruction |
|---|---|
| 3.1 | Use only the verb forms that the dictionary gives. |
| 3.2 | Use only these forms: infinitive, imperative, simple present, simple past, simple future, and past participle as adjective. |
| 3.3 | Use the past participle only as an adjective: "the cached response". |
| 3.4 | Do not use auxiliary verbs for complex constructions. No present perfect, no "is to be installed". |
| 3.5 | Use an "-ing" form only as a technical noun or inside one: "logging", "the mounting bracket". Never use it as a verb. |
| 3.6 | Use active voice. In descriptive text, passive voice is legal only when the agent is unknown. |
| 3.7 | Describe an action with a verb, not a noun: "compress the file", not "perform compression of the file". |

Approved modal verbs: `can`, `will`, `must`. Banned: `should`, `would`,
`may`, `might`, `could`. The standard rejects `could` even for possibility:
write "an explosion can occur", never "an explosion could occur". Where a
draft carries the banned modal `should`, a requirement becomes `must`, and a
recommendation is stated as fact or deleted. This distinction matters twice
over for agent instructions, because a model reads the banned modal as
optional guidance rather than a rule.

Before: "The migration has completed and the table is being rebuilt."
After: "The migration is complete. The database rebuilds the table."

Before: "The flag can be set in the config file, making restarts
unnecessary."
After: "You can set the flag in the config file. Then a restart is not
necessary."

Before: "The temperature must be adjusted."
After: "Adjust the temperature."

### Section 4 — Sentences (Rules 4.1-4.5)

| Rule | Instruction |
|---|---|
| 4.1 | Write short and clear sentences. |
| 4.2 | Do not omit words or use contractions to shorten a sentence. Keep articles. Keep "that". |
| 4.3 | Use a vertical list for complex text. |
| 4.4 | Use connecting words between sentences on related topics: "Then", "As a result". |
| 4.5 | Put an article ("the", "a", "an") or a demonstrative adjective ("this", "these") before a noun where it applies. |

Rule 4.2 is the rule against terseness. STE calls for short sentences with
complete grammar, not telegraph style.

Wrong shortening: "Make sure file exists before running."
STE form: "Make sure that the file exists before you run the command."

### Section 5 — Procedural writing (Rules 5.1-5.5)

| Rule | Instruction |
|---|---|
| 5.1 | Maximum 20 words per sentence. Warnings and cautions are included in this limit. |
| 5.2 | Write one instruction per sentence, unless two actions happen at the same time. |
| 5.3 | Write instructions in the imperative: "Run the migration." |
| 5.4 | Put a required condition before the command, and divide the two with a comma: "If the build fails, read the log." |
| 5.5 | Notes give information. Notes never give instructions. Notes take the 25-word limit. |

Before: "Grab the API key from the dashboard before you configure the
client, which you can do under Settings."
After: "Get the API key from the dashboard, under Settings. Then configure
the client with this key."

### Section 6 — Descriptive writing (Rules 6.1-6.6)

| Rule | Instruction |
|---|---|
| 6.1 | Give information gradually. Give one new fact per sentence. |
| 6.2 | Use key words and phrases to give the text a logical structure. |
| 6.3 | Maximum 25 words per sentence. |
| 6.4 | Group related information in paragraphs. |
| 6.5 | Write one topic per paragraph. |
| 6.6 | Maximum six sentences per paragraph. |

Descriptive text never takes the imperative. Descriptions explain.
Procedures instruct.

### Section 7 — Safety instructions (Rules 7.1-7.3)

| Rule | Instruction |
|---|---|
| 7.1 | Use a word that shows the risk level: "WARNING" for injury, "CAUTION" for damage. |
| 7.2 | Start with a clear command or condition. |
| 7.3 | Then give the risk or the possible result. |

Do not bury the instruction after the explanation. The pattern transfers
directly to destructive CLI flags, irreversible migrations, and dangerous
API options.

Before: "Data loss may occur in some circumstances if the destructive flag
happens to be enabled when running against production."
After: "CAUTION: Do not use the `--force` flag against production. The flag
deletes rows that do not match the source."

### Section 8 — Punctuation and word count (Rules 8.1-8.7)

| Rule | Instruction |
|---|---|
| 8.1 | All standard punctuation is legal except the semicolon. Write two sentences instead. |
| 8.2 | Use hyphens to connect words that act as one unit. |
| 8.3 | Parentheses are legal for references, item numbers, abbreviations, plural forms, explanations, and alternatives. |
| 8.4 | In a vertical list, the lead-in colon ends a sentence for word count. |
| 8.5 | Text inside parentheses counts as one word. |
| 8.6 | Count as one word each: a number, a number with a unit, an abbreviation, an alphanumeric identifier, quoted text, a title, a label, and a proper noun. |
| 8.7 | A hyphenated word counts as one word. |

Rule 8.6 matters for software text: a command in backticks is quoted text
and counts as one word for the sentence limit. A long identifier does not
use up the sentence budget.

### Section 9 — Writing practices (Rules 9.1-9.4, GR-1 to GR-8)

| Rule | Instruction |
|---|---|
| 9.1 | When a word-for-word replacement fails, restructure the sentence. |
| 9.2 | Use each approved word with its approved meaning and its approved part of speech. |
| 9.3 | Do not build phrasal verbs: "go down" becomes "decrease", "set up" becomes "install" or "configure". |
| 9.4 | Keep one consistent style and terminology through the whole document. |

General recommendations GR-1 to GR-8: keep the conjunction "that", take care
with the word "with", give every pronoun a clear referent, prefer "this" plus
a noun over a bare "this", avoid false friends between languages, avoid
Latin abbreviations, use inclusive language, and use the possessive
apostrophe form only when you are certain it is correct. GR-8 states the
fallback directly: if you are not certain, do not use the possessive
apostrophe. A non-native reader finds it hard to parse.

GR-6, applied to software docs: "e.g." becomes "for example", "i.e."
becomes "that is", and "etc." is deleted. Name the items instead, or write
"and more".

## Vocabulary discipline

The official ASD-STE100 dictionary holds roughly 900 approved words and
1,200 banned words with alternatives. It is copyrighted by ASD and is not
reproduced here. Its mechanics apply without the dictionary itself: one
word, one meaning, one part of speech.

Known part-of-speech rulings from the standard, useful as patterns:

| Word | Ruling |
|---|---|
| test, check, work | Noun only. Write "Do a test", not "test the pump". "Check that X" becomes "make sure that X". |
| oil | Noun only, as used in the examples of the standard itself. For the verb, the dictionary gives "lubricate". |
| help | Verb only. For the noun, the dictionary gives "aid": "with the aid of". |
| fall | "To move down by gravity" only. Never use it to mean "decrease". |
| follow | "To come after" only. Never use it to mean "obey". Write "obey the instructions". |
| above, below | Physical positions only. For limits write "more than" or "less than". |

### The modal ladder

| You wrote | STE form |
|---|---|
| `should` (a requirement) | `must` |
| `should` (a recommendation) | Delete it, or state it as fact: "X is better because Y." |
| `may` / `might` / `could` (possibility) | `can` |
| `may` (permission) | `can` |
| `would` (a hypothetical) | Restructure: "If X occurs, Y occurs." |

### Slop-to-simple substitutions

This table is local to this skill, not part of the ASD dictionary. It maps
words that AI-generated text overuses to plain replacements. If the word
carries no fact, delete it instead of replacing it.

| Slop | Write instead |
|---|---|
| leverage, utilize | use |
| in order to | to |
| prior to | before |
| ensure | make sure that |
| it is worth noting that | (delete) |
| it is important to, crucially | (delete — state the fact) |
| simply, just, easily, seamlessly, effortlessly | (delete) |
| robust, powerful, comprehensive, performant | (delete, or give the measurable property) |
| functionality | function, feature |
| enables you to, allows you to | you can |
| is designed to, aims to | (delete — say what it does) |
| facilitate | help, make possible |
| dive into, delve into | read, examine |
| when it comes to | for |
| in the event that | if |
| due to the fact that | because |
| as needed, as necessary | (state the condition) |
| and/or | Pick one, or write "X, or Y, or both" |
| e.g. / i.e. / etc. | for example / that is / (name the items) |
| gracefully handles | (say what it does: "retries three times, then stops") |
| out of the box | by default |
| under the hood | internally |
| blazingly fast, state-of-the-art | fast, with the number given, or delete it |
| streamline | make simpler, make faster |
| plethora, myriad | many |
| addresses the issue, tackles | corrects the fault, removes the error |

### Consistency pass

Collapse each of these rotations to one term (Rules 1.11, 9.4):

- check / verify / confirm / validate / ensure: pick one
- config / configuration / settings / options: pick one
- delete / remove / drop / destroy: one term per meaning, kept consistent
- error / issue / problem / failure: "error" for an error, "failure" for a
  failed operation
- run / execute / invoke / launch: pick one
- show / display / render / present: pick one

## Untouchables

These are technical names, per Rules 1.5 and 8.6. Leave them exact, even
when they break a vocabulary rule:

- Code blocks, inline code, identifiers, CLI commands, flags, and file
  paths
- Quoted error messages and log lines
- Product names, API endpoint names, and config keys
- A number with a unit — each counts as one word in the sentence limit

## Beyond documentation

The same rules apply to different targets. Full adaptations live in
`references/use-cases.md`.

- Error messages: state what happened, in simple past. State the cause if
  known. Give the fix as an imperative. No apology filler.
- Runbooks: the home ground of STE. Imperative steps, conditions first,
  warnings before the step.
- Incident reports: simple past only. "We have identified an issue that may
  have impacted" becomes "Between 14:02 and 14:31 UTC, 12% of requests
  failed."
- Release notes: a breaking change follows the warning pattern, command
  first, risk second.
- Agent instructions (prompts, AGENTS.md files, other skills): a system
  prompt is a procedure for a reader that cannot ask a question. One
  instruction per sentence. No banned modal. Condition first.
- Translation preparation: the original purpose of STE. One meaning per
  word plus complete grammar removes most translation ambiguity.

## Self-check before you deliver

This step is mandatory. Run these four checks on your draft.

1. Count the words in your three longest sentences. Split any sentence over
   the 20-word or 25-word limit.
2. Search your draft for a contraction mark, `has been`, `have been`, the
   banned modal `should`, an "-ing" verb after a comma, and a semicolon.
3. Search for every "if" and every "when". Each one must stand at the start
   of its sentence, before the command: "Increase the timeout if the network
   is slow" becomes "If the network is slow, increase the timeout."
4. Search for every verb you did not pick in step 4 of "Your task" (the
   check/verify/confirm set). Replace every hit with your chosen verb.

Fix what you find, then deliver the text. For a full audit, run the pass in
`references/checklist.md`.

## Full example

Before (real, unedited AI output):

> Connection timeouts. If sqlpipe hangs or fails with `dial tcp: i/o
> timeout`, check that the host running sqlpipe can reach the Postgres port
> (usually 5432) — this is often a security group or firewall rule blocking
> the connection. If you're connecting to a managed database (RDS, Cloud
> SQL, etc.), confirm the instance allows connections from sqlpipe's IP. You
> can also try increasing `source.connect_timeout_seconds` in your config,
> since a slow network path can trip the default timeout even when the
> connection eventually succeeds.

After (classified as procedural, chosen verb "make sure", conditions moved
first, one instruction per sentence):

> Connection timeouts. sqlpipe stops with `dial tcp: i/o timeout` when it
> cannot reach the Postgres port, 5432 by default.
>
> 1. Make sure that the host that runs sqlpipe can reach the Postgres port.
>    A firewall or security group usually blocks it.
> 2. If the database is managed (RDS, Cloud SQL), make sure that the
>    instance accepts connections from the IP address of sqlpipe.
> 3. If the network is slow, increase `source.connect_timeout_seconds` in
>    the configuration.

What changed: the longest sentences split under the 20-word limit. The
contraction "you're" expanded. "check" and "confirm" collapsed to "make sure
that". Every condition moved before its command. "etc." was removed. Code
and the error string stayed untouched.

## Limits

STE governs technical facts and instructions. Do not apply it to marketing
copy, brand writing, or launch posts — it removes persuasion by design. When
a user asks for STE on marketing text, say so, and offer STE for the
supporting docs instead.

## Attribution and disclaimer

This skill adapts the rule catalogue, the modal ladder, the slop
substitution table, and the use-case list from
[AminBlg/SimpleEnglish](https://github.com/AminBlg/SimpleEnglish), used
under its MIT licence. The three-class register model and the Te Reo
framing-position rule are new material, written for this repository.

ASD-STE100 is a registered trademark of ASD. This skill is unofficial. It is
not affiliated with, and is not endorsed by, ASD or STEMG. No tool,
including this skill, can guarantee ASD-STE100 compliance. The official
standard is a free download at asd-ste100.org.

## References

- `references/checklist.md` — a full verification pass with searchable
  patterns, for check mode and for final audits
- `references/use-cases.md` — long-form adaptations: error messages,
  runbooks, incident reports, commit messages, UI copy, and translation
  preparation
