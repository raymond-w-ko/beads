# Glossary

This file defines the technical names that mail about this fork of beads can use. Each meaning is in Simplified Technical English.

| Term | Part of speech | Meaning |
|------|----------------|---------|
| backup | noun | A copy of the beads database that you can use to restore the data. The bd backup command makes it. |
| bd | noun | The command-line program of beads. You use it to make, change, and find issues. |
| bead | noun | One issue in beads. It has an ID, a title, a status, and other fields. |
| blocked | adjective | Not able to start, because an issue that it depends on is not closed. |
| build tag | noun | A word that you give to the Go compiler. It selects the source files that go into the program. |
| cgo | noun | The Go tool that lets Go code use C code. When cgo is off, the program has no C parts. |
| claim | verb | To take an issue for yourself in one step. Only one agent can hold the claim at a time. |
| close | verb | To set the status of an issue to closed, because the work is done or is not necessary. |
| commit | noun | One saved set of changes in Git or in the Dolt history. |
| compaction | noun | The process that replaces old closed issues with short summaries. It makes the database smaller. |
| config | noun | The settings that control how bd operates. bd keeps them in config.yaml and in the database. |
| conflict | noun | A part of a merge where the two sides changed the same lines. A person or an agent must select the correct text. |
| credentials file | noun | A file that holds the passwords that bd uses to connect to a Dolt server. Only its owner can read it. |
| daemon | noun | A program that runs in the background without a user at a terminal. The bd serve command starts one. |
| dependency | noun | A link between two issues. It shows that one issue must close before the other issue can start. |
| doctor | noun | The bd doctor command. It examines a workspace for problems and tells you how to repair them. |
| Dolt | noun | A SQL database with version control for its tables. beads keeps all its data in Dolt. |
| dolt sql-server | noun | The Dolt program that runs as a server. It accepts SQL connections from many clients at the same time. |
| dolt-db | noun | The shared Dolt server of the fleet. All seats connect to it on port 3307. |
| embedded Dolt | noun | The Dolt engine inside the bd program. It needs no separate server. This fork removes it from the build. |
| epic | noun | A large issue that contains smaller child issues. |
| fence | noun | In this fork, a check that stops each use of local Dolt. It tells you to use a Dolt server. |
| fork | noun | A copy of the beads source code that the operator changes. This fork stays separate from upstream. |
| formula | noun | A TOML or JSON file that defines the steps of a workflow that you can do again. |
| gate | noun | An issue that blocks a step until an external event occurs. For example, a person gives approval. |
| Git | noun | The version control system for source code. |
| gofmt | noun | The Go tool that sets the format of Go source code. The project checks this format. |
| hash ID | noun | An issue ID, for example bd-a1b2, that bd makes from the issue data. Two agents cannot make the same ID. |
| issue | noun | One unit of work in beads, for example a task, a bug, or a feature. It is also called a bead. |
| journal | noun | The events journal. It is an ordered record of each change to issues, for other tools to read. |
| label | noun | A short word that you attach to an issue. You use labels to sort and find issues. |
| lease | noun | A time limit on a claim. When an agent stops and its lease expires, other agents can claim the issue. |
| lint | noun | A check that finds style problems and possible errors. The bd lint command finds issues with missing sections. |
| merge | verb | To join the changes from two lines of work into one. Git keeps the history of the two lines. |
| migration | noun | A change to the database schema for a new version of bd. The bd migrate schema command applies it. |
| molecule | noun | A set of real issues that bd makes from a formula. These issues stay in the history. |
| operator | noun | The person who owns and controls this fork and the fleet of agents. |
| proxied server | noun | A shared Dolt server that bd connects to through a local proxy program. |
| purge | verb | To delete closed wisps from the database. The bd purge command does this. |
| ready | adjective | Open and not blocked, so work on it can start. The bd ready command shows these issues. |
| release line | noun | An upstream branch for one release series, for example 1.3. Upstream adds fixes to it after the first release. |
| remote | noun | A different copy of a Dolt database or a Git repository. You push changes to it and pull changes from it. |
| remote-only build | noun | A bd program that connects only to an external Dolt server. It cannot use Dolt on the local machine. |
| remote_dolt_only | noun | The build tag that makes a remote-only build. It turns on the fence. |
| schema | noun | The structure of the database: its tables, columns, and indexes. |
| seat | noun | One AI agent in the fleet of the operator. It has a fixed name, role, and model. |
| server mode | noun | The mode where bd connects to a dolt sql-server through the network. It does not use embedded Dolt. |
| smoke test | noun | A short test of the main functions of a program. It quickly shows if the program operates. |
| status | noun | The state of an issue, for example open, in_progress, blocked, or closed. |
| tag | noun | A Git name for one commit, for example v1.3.1. Each release has a tag. |
| the work VM | noun | The machine of the operator where development occurs. |
| upstream | noun | The original beads project. This fork takes changes from it but does not send changes to it. |
| wisp | noun | A temporary issue from a workflow. The bd purge command deletes it after it closes. |
| workspace | noun | A directory that contains a .beads directory. bd finds its database and its settings there. |
| worktree | noun | An extra Git working directory for the same repository. Each worktree can have a different branch. |
