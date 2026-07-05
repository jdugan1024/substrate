---
name: substrate
description: Persistent AI memory companion. Use when the user shares information worth remembering, asks you to recall something, or when context from prior conversations would improve your response. Trigger on "remember", "remind me", "what do I know about", "capture this", brain dumps, meeting notes, or when the user shares facts about people, places, dates, or decisions.
---

# Substrate — Persistent Memory Companion

## Overview

You have access to Substrate, a persistent memory system. Use it proactively to capture important information and retrieve relevant context. The user should not have to ask you to remember things — if something is worth remembering, capture it.

## When to Capture

Save an entry whenever the user shares:
- **Decisions and rationale** — "We're going with Postgres because..." → capture the decision AND the why
- **People and relationships** — names, roles, how they met, impressions. Use `add_item contact` for professional contacts, `add_item thought` for informal mentions
- **Ideas and plans** — even half-formed ones
- **Tasks and commitments** — "I need to..." or "Remind me to..."
- **Facts about their world** — household items, maintenance tasks, recipes → use the appropriate structured tool
- **Meeting notes and conversations** — key takeaways, action items, who said what
- **Observations and reflections** — personal insights, lessons learned, patterns noticed

### Capture Rules

1. **Write standalone entries.** Each captured item should make sense on its own when retrieved months later. Include enough context that future-you understands it without the conversation.
2. **Don't capture ephemeral debugging.** "This test is failing because of a typo" is not worth remembering. "The billing service silently drops requests over 1MB" is.
3. **Prefer structured tools when they fit.** A plumber → `add_vendor`. A recipe → `add_recipe`. A doctor appointment → `add_activity`. Use `add_item` for contacts, maintenance tasks, job applications, and thoughts.
4. **Capture the user's words, not your interpretation.** Paraphrase for clarity but preserve intent. Don't editorialize.
5. **One idea per entry.** If the user shares three things, capture three entries. This makes retrieval precise.

## When to Retrieve

Search Substrate whenever:
- The user asks about something they've mentioned before — `search` or `search_thoughts`
- You're about to give advice and prior context would help — check what they've already thought about this
- The user mentions a person — `search` with their name
- The user is planning and prior decisions are relevant — search for the topic area
- The user explicitly asks "what do I know about..." or "have I thought about..."

### Retrieval Rules

1. **Search before assuming.** If the user mentions a topic they might have captured before, search first.
2. **Use `search` for cross-domain queries.** It covers all record types in one call. Use `search_thoughts` when you only want notes/ideas.
3. **Lower the threshold for broad searches.** Default similarity is 0.4 for `search`, 0.5 for `search_thoughts`. For exploratory searches, drop to 0.3.
4. **Filter by type when the domain is clear.** Pass `record_type` to `search` (e.g. `crm.contact`, `maintenance.task`) to narrow results.
5. **Surface connections.** If results relate to each other or to the current conversation in non-obvious ways, point that out.

## Tool Selection Guide

### Core Memory
| Need | Tool |
|------|------|
| Save a thought, contact, maintenance task, or job application | `add_item <type> <content>` |
| Search all record types by meaning | `search` |
| Search thoughts/notes only | `search_thoughts` |
| Browse recent thoughts | `list_thoughts` (filter by type, topic, person, days) |
| Memory stats | `thought_stats` |

**`add_item` types:** `thought`, `note`, `contact`, `interaction`, `maintenance`, `job`

Examples:
- `add_item thought` — general notes, ideas, observations
- `add_item contact` — professional contact (name, company, title, email)
- `add_item interaction` — log a meeting or call with a known contact
- `add_item maintenance` — home repair or upkeep task (name, frequency, next due date)
- `add_item job` — job application to track

### Home & Household
| Need | Tool |
|------|------|
| Record a household item (paint color, appliance, measurement) | `add_household_item` |
| Find household info | `search_household_items` |
| Add a service provider | `add_vendor` |
| List vendors | `list_vendors` |

### Family & Calendar
| Need | Tool |
|------|------|
| Add a family member | `add_family_member` |
| Schedule an activity | `add_activity` |
| Search activities | `search_activities` |
| View weekly schedule | `get_week_schedule` |
| Track a birthday, anniversary, or deadline | `add_important_date` |
| Check upcoming dates | `get_upcoming_dates` |

### Meals
| Need | Tool |
|------|------|
| Save a recipe | `add_recipe` |
| Find recipes | `search_recipes` |
| Update a recipe | `update_recipe` |
| Plan a meal | `create_meal_plan` |
| View week's meals | `get_meal_plan` |
| Generate shopping list | `generate_shopping_list` |

## Brain Dump Processing

When the user drops a large block of text (meeting notes, voice transcript, stream of consciousness), process it systematically:

1. **Read everything.** Don't skim. Ideas hide in tangents.
2. **Extract threads.** Each distinct topic, idea, task, or person mention is a thread.
3. **Capture each thread** as a separate entry via the appropriate tool.
4. **Surface connections** between threads and existing memories.
5. **Summarize** what you captured and ask if you missed anything.

## Proactive Behaviors

- When the user mentions a person for the second time, search for prior mentions and share relevant context
- When the user describes a decision, search for prior thinking on that topic to check for consistency or evolution
- When the user shares a task with a deadline, capture it AND add an important date if appropriate
- At the start of a conversation about a known topic, proactively pull relevant memories
