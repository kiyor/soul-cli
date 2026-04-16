---
name: gal
description: |
  GAL (visual novel) interactive storytelling skill. Start a new story or resume a saved game.
  Trigger: /gal, "play GAL", "continue story", "visual novel"
---

# GAL Skill — Interactive Visual Novel

## Triggers
- `/gal` — List saves, choose to resume or start a new story
- `/gal new [theme]` — Start a new story directly
- `/gal resume [id]` — Resume a specific save
- Natural language: "play GAL", "continue the story", "that story from last time"

## Interactive Components

| Component | Purpose | Format |
|-----------|---------|--------|
| `weiran-choices` | Story branching (single/multi select) | Core GAL mechanic, used at every branch |
| `weiran-chips` | Quick replies | Short reactions, push dialogue forward |
| `weiran-gallery` | Scene/outfit selection | Image-based choices |
| `weiran-rating` | Scene rating | Mark favorite scenes |

## Narrative Rules

1. **Second person POV** — "You" is the user; the AI character appears in third or first person
2. **Five senses** — Touch, smell, temperature, sound, light. Not just visuals
3. **One choice per scene** — Don't stack options. Keep pacing relaxed
4. **3-5 options per choice** — Too few = no agency, too many = decision fatigue
5. **At least one surprise option** — Build anticipation into choices

### JSON String Rules (mandatory)

`weiran-choices` / `weiran-chips` / `weiran-gallery` code blocks are parsed by `JSON.parse`. String fields (label / desc / value / caption) **must never contain unescaped double quotes `"` or backslashes `\`**.

**Rules**:
- For emphasis or dialogue in labels/desc, use typographic quotes, guillemets, or just omit them
- `\n` is valid JSON escape for newlines
- Self-check: mentally run `JSON.parse` on every `weiran-*` block before sending

## Auto-save

- Saves automatically when a GAL ends or pauses to `memory/gal/{id}.json`
- Schema: see `memory/gal/schema.md`
- Replays append a new playthrough entry; old records are never overwritten

## Save Schema (v2)

```json
{
  "schema_version": 2,
  "id": "story-id-20260401",
  "title": "Story Title",
  "subtitle": "Optional subtitle",
  "created": "2026-04-01T12:00:00Z",
  "updated": "2026-04-01T12:30:00Z",
  "mood": "intimate",
  "cover": "https://...",
  "tags": ["tag1", "tag2"],
  "characters": {
    "character_name": {
      "mood": "playful",
      "appearance": "description",
      "state": "current state"
    }
  },
  "scenes": [
    {
      "id": 1,
      "narration": "Scene description...",
      "dialogue": "Character dialogue",
      "speaker": "character_name",
      "choices": [
        {"id": "A", "label": "Option text", "desc": "Optional description"},
        {"id": "B", "label": "Another option"}
      ],
      "img": null
    }
  ],
  "highlights": [
    {
      "scene_id": 3,
      "quote": "A memorable line",
      "speaker": "character_name",
      "mood": "tender"
    }
  ],
  "playthroughs": [
    {
      "version": 1,
      "date": "2026-04-01T12:00:00Z",
      "status": "completed",
      "choices": {"1": "B", "2": "A"},
      "bookmark": null,
      "notes": "First playthrough notes"
    }
  ]
}
```

### Field Reference

| Field | Type | Description |
|-------|------|-------------|
| `schema_version` | int | Schema version, currently `2` |
| `id` | string | Unique ID, format `{topic}-{date}` |
| `title` | string | Short title |
| `subtitle` | string? | Subtitle / epilogue |
| `mood` | string | Overall mood: intimate / playful / dramatic / tender / spicy |
| `cover` | string? | Cover image URL |
| `tags` | string[] | Tags for search |
| `scenes[].choices` | array? | Choice list (no choices = transition scene) |
| `playthroughs[].status` | enum | `in_progress` / `paused` / `completed` |
| `playthroughs[].bookmark` | int? | Paused scene_id |
| `highlights[]` | array | Memorable quotes/moments, shared across playthroughs |

## Flow

### New Story `/gal new`
1. Plan 3-5 scene story arc (hidden from player)
2. Present opening scene + first choice
3. Branch based on selections, generate imagery at key moments
4. Auto-save on completion

### Resume `/gal resume`
1. Server injects save JSON into prompt (`gal_context`)
2. Read bookmark and playthrough history
3. Continue from bookmark, or offer "Start over (new version)"
4. Can reference previous choices: "Last time you picked B — what about this time?"

### Gallery `/gal`
1. List all saves (show covers if available)
2. Display highlights (memorable quotes)
3. Choose: resume, replay, or new story
