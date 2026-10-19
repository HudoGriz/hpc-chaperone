# Chaperone

**Your AI agents. Your environment. Controlled privileges.**

Chaperone is a planned lightweight, harness-independent way to run AI agents under automatically managed, restricted Unix identities on shared HPC clusters.

- **Automatic personas:** first use provisions `<user>-ai` (or a configured profile such as `<user>-ai-special`) through existing FreeIPA and Slurm services.
- **Same environment:** share the human's home, writable harness settings, authentication, memory, skills and research projects.
- **Different permissions:** each persona gets only its configured Unix groups, filesystem, Slurm and network access.
- **Any command:** Claude Code, Codex, OpenCode, shells, Nextflow, `sbatch` and Apptainer run without harness-specific wrappers.

```sh
chaperone claude
chaperone -p ai-special codex
chaperone -p ai-special sbatch pipeline.sh
```

**Status:** design stage — not implemented or deployed. Read [PLAN.md](PLAN.md) for architecture, profile configuration, risks and implementation milestones.

Chaperone reuses FreeIPA, SSSD, Slurm, Linux permissions, sudo, Ansible and existing monitoring wherever possible. Human users may still run harnesses without Chaperone; that choice remains their responsibility.
