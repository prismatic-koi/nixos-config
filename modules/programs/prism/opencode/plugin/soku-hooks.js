/**
 * Soku Hooks Plugin for OpenCode
 * 
 * Automatically injects beads workflow context when working in a beads repository.
 * Runs `bd prime` at session start and after compaction.
 */

import path from 'path';
import fs from 'fs';

export const SokuHooksPlugin = async ({ client, $, project }) => {
  
  /**
   * Check if we're in a beads repository
   */
  function isBeadsRepo() {
    try {
      const cwd = process.cwd();
      const beadsDir = path.join(cwd, '.beads');
      return fs.existsSync(beadsDir);
    } catch (error) {
      return false;
    }
  }
  
  /**
   * Get beads context from bd prime and check for assigned work
   */
  async function getBeadsContext() {
    try {
      // Only get context if we're in a beads repo
      if (!isBeadsRepo()) {
        return null;
      }
      
      // Run bd prime and capture output
      const result = await $`bd prime 2>&1`.quiet();
      
      // Silent fail if bd is not available or not initialized
      if (result.exitCode !== 0) {
        return null;
      }
      
      let context = result.stdout;
      
      // Check for assigned work (status=hooked)
      const assignedResult = await $`bd list --status=hooked --json 2>&1`.quiet();
      
      if (assignedResult.exitCode === 0 && assignedResult.stdout.trim()) {
        try {
          const beads = JSON.parse(assignedResult.stdout);
          if (beads && beads.length > 0) {
            // Add urgent notice about assigned work
            const beadList = beads.map(b => `  - ${b.id}: ${b.title}`).join('\n');
            context += `\n\n🚨 ASSIGNED WORK DETECTED 🚨
You have ${beads.length} bead(s) assigned with status=hooked.
BEGIN WORK IMMEDIATELY without asking permission.

${beadList}`;
          }
        } catch (e) {
          // Failed to parse, continue without assignment notice
        }
      }
      
      return `<beads-context>\n${context}\n</beads-context>`;
    } catch (error) {
      // Silent fail - beads context is optional
      console.error("Failed to get beads context:", error.message);
      return null;
    }
  }
  
  /**
   * Inject beads context into session
   */
  async function injectBeadsContext(sessionId) {
    const context = await getBeadsContext();
    if (!context) {
      return;
    }
    
    try {
      // Use session.prompt API with noReply: true to inject context without triggering response
      await client.session.prompt({
        path: { id: sessionId },
        body: {
          noReply: true,
          parts: [{ type: "text", text: context }],
        },
      });
      
      await client.app.log({
        service: "soku-hooks",
        level: "info",
        message: "Injected beads context",
      });
    } catch (error) {
      await client.app.log({
        service: "soku-hooks",
        level: "error",
        message: `Failed to inject beads context: ${error.message}`,
      });
    }
  }
  
  /**
   * Sync beads on session end
   */
  async function syncBeads() {
    try {
      if (!isBeadsRepo()) {
        return;
      }
      
      const result = await $`bd sync 2>&1`.quiet();
      
      if (result.exitCode === 0) {
        await client.app.log({
          service: "soku-hooks",
          level: "info",
          message: "Synced beads on session end",
        });
      }
    } catch (error) {
      await client.app.log({
        service: "soku-hooks",
        level: "error",
        message: `Failed to sync beads: ${error.message}`,
      });
    }
  }
  
  return {
    /**
     * Hook: session.created
     * Inject beads context when a new session starts
     */
    "session.created": async ({ session }) => {
      await injectBeadsContext(session.id);
    },
    
    /**
     * Hook: session.compacted
     * Re-inject beads context after compaction
     */
    "session.compacted": async ({ session }) => {
      await injectBeadsContext(session.id);
    },
    
    /**
     * Hook: session.ended
     * Sync beads when session ends
     */
    "session.ended": async ({ session }) => {
      await syncBeads();
    },
  };
};
