# Slop Cannon

The idea behind this project is a command line interface / TUI which you can fire at a project. 

You will watch it generate a graph of task and dependencies and agents and feedback loops and revieve any prs etc that get pushed. 

you will see cool cannon animations when you watch it from the TUI. 

## Ammo breakdown

1. engineer
2. UX / QA / TESTER
3. product owner / business analyst (deeply researches the project / folder and comes up with ideas based on what's inside and things to add or fix and makes beads for the agents to work on)
4-inf → add more of 1 & 2. Ideally, the split will be three testers for each implementor to give feedback. The feedback will loop untill all testors are satisfied. an engineer should be able to push back on tester feedback if needed.

```
sc ./repo-path 

sc ./repo-path -m "here's what I want done"

sc cartridge --add claude --model fable
sc cartridge --remove claude
sc cartridge --add animus
sc cartridge --add kimi --model 3
sc ammo 12
sc vibes --add vibes.md
sc rules --add rules.md
sc safety off
```

safety = PRs or straight to main
cartridge = cli subscription which gets used
ammo = number of sub agents (minmum 3)
vibes = stylistic choices which get communicated to all agents
rules = rules which get communicated to all agents
-m / message = start work with this brief
just a repo path with no parameters = full auto pilot

defaults
- safety on
- ammo 10
- vibes/rules - none
- cartridge is always blank you need to give it a subscription for it to work

# dependencies 
- beads / dolt
- some kind of cli llm subscription