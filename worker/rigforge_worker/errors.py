class PermanentError(Exception):
    """A failure that retrying cannot fix: the input itself is the problem
    (not a video, no person in frame, unknown character).

    The worker dead-letters these immediately instead of spending the retry
    budget on them. Any other exception is treated as transient (network,
    storage, out of disk) and goes through retry with backoff.
    """
