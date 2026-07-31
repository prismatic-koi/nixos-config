# Verification checklist

Run this pass on every draft before you deliver it. The checks move from
mechanical to judgment.

## Mechanical checks (searchable)

Search the draft for each pattern. Every hit outside a code block and outside
quoted text is a violation.

| Search for | Violation | Fix |
|---|---|---|
| a contraction mark (`'ll`, `'re`, `'ve`, `n't`, `it's`) | Contraction (Rule 4.2) | Expand it. |
| `has been`, `have been`, `had been` | Present perfect or past perfect (Rule 3.4) | Simple past or simple present. |
| `has` or `have` plus a past participle | Present perfect (Rule 3.4) | Simple past. |
| the banned modal `should`, `would`, `may`, `might`, `could` | Unapproved modal (Rule 3.2) | See the modal ladder in SKILL.md. |
| `is being`, `are being`, `was being` | Progressive passive (Rules 3.4, 3.5) | Active voice, simple tense. |
| `, making`, `, allowing`, `, enabling`, `, ensuring` | An "-ing" clause used as a verb (Rule 3.5) | New sentence, with a real subject. |
| a semicolon | Semicolon (Rule 8.1) | Two sentences. |
| `e.g.`, `i.e.`, `etc.` | Latin abbreviation (GR-6) | "for example", "that is", or name the items. |
| `simply`, `easily`, `seamlessly`, `robust` | Filler that carries no fact | Delete. |
| a mid-sentence `if` or `when` | Trailing condition (Rule 5.4) | Move the condition to the start of the sentence, and add a comma. |

## Countable checks

1. Sentence length. Count the words in each sentence. Procedural limit: 20
   words. Descriptive limit: 25 words. Note limit: 25 words. A backticked
   command, a number with a unit, and an identifier each count as one word
   (Rule 8.6).
2. Paragraph size. Maximum six sentences per paragraph (Rule 6.6).
3. Multi-word nouns. For any noun chain over three words, break it with a
   preposition (Rule 2.1).
4. Instructions per sentence. One instruction per sentence, unless the
   actions are simultaneous (Rule 5.2).

## Judgment checks

5. Classification. Is each passage cleanly procedural or descriptive? A
   procedure takes the imperative. A description never takes the
   imperative.
6. Voice. For any passive sentence, is the agent truly unknown, and is the
   passage descriptive? If not, make it active (Rule 3.6).
7. Condition placement. Every "if" or "when" stands before its command, with
   a comma (Rule 5.4).
8. Synonym rotation. One term per concept, through the whole document
   (Rules 1.11, 9.4). Scan for check/verify/confirm and for
   config/settings, and for run/execute.
9. Warnings. Command or condition first, risk second (Rules 7.2, 7.3).
10. Completeness. Articles present. "that" present after "make sure". No
    telegraph style (Rule 4.2).
11. Untouchables intact. Code, identifiers, quoted errors, and proper nouns
    are unchanged.
12. Register class. If the passage is class B, confirm no Te Reo term sits
    inside the sentence that states the finding, the risk, the option, or
    the recommendation. Run the deletion cross-check from SKILL.md.

## When you report violations (check mode)

For each violation, give the rule number, the offending text, and a
compliant rewrite. Cite only a rule number that appears in SKILL.md.

When the user asked for STE compliance, end the report with this statement:
"No tool can guarantee ASD-STE100 compliance. Final approval rests with the
writer. The official standard is a free download at asd-ste100.org."
