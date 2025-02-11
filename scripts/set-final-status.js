// @ts-check
const core = require('@actions/core');
const github = require('@actions/github');

async function run() {
  try {
    const context = 'ci-tests';
    const sha = process.env.GITHUB_SHA;

    // Collect job results from GitHub context
    const jobResults = Object.entries(github.context.job)
      .filter(([jobName, jobData]) => 
        jobName !== 'set-pending-status' && 
        jobName !== 'final-status-check' && 
        typeof jobData === 'object' && 
        jobData.result
      )
      .map(([jobName, jobData]) => jobData.result);

    const hasFailed = jobResults.some(result => 
      result === 'failure' || 
      result === 'timed_out' || 
      result === 'cancelled'
    );

    const state = hasFailed ? 'failure' : 'success';
    const description = hasFailed ? 'CI workflow failed' : 'CI workflow passed';

    const octokit = github.getOctokit(core.getInput('token'));

    await octokit.rest.repos.createCommitStatus({
      owner: 'syntasso',
      repo: 'kratix',
      sha: sha,
      state: state,
      target_url: `https://github.com/syntasso/ci/actions/runs/${github.context.runId}`,
      description: description,
      context: context
    });

    core.info(`Successfully set final commit status to ${state}`);
  } catch (error) {
    core.setFailed(`Action failed with error ${error}`);
  }
}

run();
